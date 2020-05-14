package redfish

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	resty "github.com/go-resty/resty/v2"
	"github.com/pkg/errors"

	"github.com/go-logr/logr"
	logf "sigs.k8s.io/controller-runtime/pkg/runtime/log"

	metal3v1alpha1 "github.com/metal3-io/baremetal-operator/pkg/apis/metal3/v1alpha1"
	"github.com/metal3-io/baremetal-operator/pkg/bmc"
	"github.com/metal3-io/baremetal-operator/pkg/provisioner"
)

/*
  Design qustions
  a. Should there be descrete steps like
     - Register
     - Inspected
     - Ready
     - Provision ->
  b. This will require the curent for loop to lookup only Ready nodes to kickstart tha actual cycle
  c. Need new APIs
      - PowerOn
      - Power Off
  d. Should the APIs be routed via protobuf server or directly interact  with drivers.
  e. Validation of Nodes in terms of the Node’s driver has enough information to manage the Node. This polls each
     interface on the driver, and returns the status of that interface.

  Assumptions:
  a. resty latest version fail POS with 307. So downgraded it to v2.0.0




*/

// Provisioner implements the provisioning.Provisioner interface
// and uses metamorph provisioner to manage the host.
type redfishProvisioner struct {
	// the host to be managed by this provisioner
	host *metal3v1alpha1.BareMetalHost
	// a shorter path to the provisioning status data structure
	status *metal3v1alpha1.ProvisionStatus
	// access parameters for the BMC
	bmcAccess bmc.AccessDetails
	// credentials to log in to the BMC
	bmcCreds bmc.Credentials
	// a client for talking to metamorph
	client *resty.Client
	// a logger configured for this host
	log logr.Logger
	// an event publisher for recording significant events
	publisher provisioner.EventPublisher
}

func LogStartup() {
	log.Info("Redfish settings",
		"endpoint", metamorphEndpoint,
	)
}

var log = logf.Log.WithName("baremetalhost_redfish")
var deprovisionRequeueDelay = time.Second * 10
var provisionRequeueDelay = time.Second * 10
var powerRequeueDelay = time.Second * 10
var introspectionRequeueDelay = time.Second * 15
var metamorphEndpoint string

func init() {
	metamorphEndpoint = os.Getenv("METAMORPH_ENDPOINT")
	if metamorphEndpoint == "" {
		fmt.Fprintf(os.Stderr, "Cannot start: No METAMORPH_ENDPOINT variable set\n")
		os.Exit(1)
	}
}

func (p *redfishProvisioner) createNodeInMetamorph() (string, error) {

	nodeIPAddress := strings.Split(p.bmcAccess.DriverInfo(p.bmcCreds)["redfish_address"].(string), "/")
	log.Info("Node IPMI IP address retrieved ", nodeIPAddress[2])
	userName := p.bmcCreds.Username
	password := p.bmcCreds.Password

	crdinfoString := " { \"name\" : \"%v\"," +
		" \"imageURL\" : \"%v\", " +
		" \"checksumURL\" : \"%v\", " +
		" \"IPMIUser\": \"%v\", " +
		" \"IPMIPassword\" : \"%v\", " +
		" \"IPMIIP\" : \"%v\", " +
		" \"disableCertVerification\" : %v }"

	crdinfoString = fmt.Sprintf(crdinfoString,
		p.host.ObjectMeta.Name,
		p.host.Spec.Image.URL,
		p.host.Spec.Image.Checksum,
		userName,
		password,
		nodeIPAddress[2],
		p.host.Spec.BMC.DisableCertificateVerification,
	)
	log.Info(crdinfoString)

	resp, err := PostRequestToMetamorph(metamorphEndpoint+"node", []byte(crdinfoString))

	if err != nil {
		return "", err
	}
	nodeUUID := resp["result"]
	log.Info(fmt.Sprintf("Node UUID retrieved %v\n", nodeUUID))

	return nodeUUID.(string), nil
}

func newProvisioner(host *metal3v1alpha1.BareMetalHost, bmcCreds bmc.Credentials, publisher provisioner.EventPublisher) (*redfishProvisioner, error) {

	bmcAccess, err := bmc.NewAccessDetails(host.Spec.BMC.Address, host.Spec.BMC.DisableCertificateVerification)

	if err != nil {
		return nil, errors.Wrap(err, "failed to parse BMC address information")
	}
	//Check if metamorph  API is accessible

	_, err = GetRequestToMetamorph(metamorphEndpoint + "nodes")
	if err != nil {
		return nil, errors.Wrap(err, "failed to connect to Metamorph Endpoint")
	}

	p := &redfishProvisioner{
		host:      host,
		status:    &(host.Status.Provisioning),
		bmcAccess: bmcAccess,
		bmcCreds:  bmcCreds,
		client:    resty.New(),
		log:       log.WithValues("host", host.Name),
		publisher: publisher,
	}
	return p, nil

}

func New(host *metal3v1alpha1.BareMetalHost, bmcCreds bmc.Credentials, publisher provisioner.EventPublisher) (provisioner.Provisioner, error) {
	return newProvisioner(host, bmcCreds, publisher)
}

func PutRequestToMetamorph(endpointURL string, data []byte) (map[string]interface{}, error) {
	resultBody := make(map[string]interface{})
	restyClient := resty.New()

	resp, err := restyClient.R().EnableTrace().
		SetHeader("Content-Type", "application/json").
		SetBody([]byte(data)).Put(endpointURL)

	if (err == nil) && (resp.StatusCode() == http.StatusOK) {
		err = json.Unmarshal(resp.Body(), &resultBody)
	} else {
		log.Info("Trace info:", resp.Request.TraceInfo())
		return nil, errors.Wrap(err, fmt.Sprintf("Put request failed : URL - %v, reqbody - %v", endpointURL, string(data)))
	}
	return resultBody, err
}

func CheckPutRequestToMetamorph(nodeUUID string, data []byte) (err error) {

	response := make(map[string]interface{})
	if response, err = PutRequestToMetamorph(metamorphEndpoint+"node/"+nodeUUID, []byte(data)); err != nil {
		err = errors.Wrap(err, "failed to update node with UUID :"+nodeUUID)
	}
	if !strings.Contains(response["result"].(string), "Update successful") {
		err = errors.Wrap(err, "failed to update node with UUID :"+nodeUUID)
	}
	return
}
func PostRequestToMetamorph(endpointURL string, data []byte) (map[string]interface{}, error) {
	resultBody := make(map[string]interface{})
	restyClient := resty.New()

	resp, err := restyClient.R().EnableTrace().
		SetHeader("Content-Type", "application/json").
		SetBody([]byte(data)).Post(endpointURL)

	if (err == nil) && (resp.StatusCode() == http.StatusOK) {
		err = json.Unmarshal(resp.Body(), &resultBody)
	} else {
		log.Info("Trace info:", resp.Request.TraceInfo())
		return nil, errors.Wrap(err, fmt.Sprintf("Post request failed : URL - %v, reqbody - %v", endpointURL, string(data)))
	}
	return resultBody, err
}

func GetRequestToMetamorph(endpointURL string) (map[string]interface{}, error) {
	resultBody := make(map[string]interface{})
	restyClient := resty.New()

	resp, err := restyClient.R().EnableTrace().Get(endpointURL)

	if (err == nil) && (resp.StatusCode() == http.StatusOK) {
		err = json.Unmarshal(resp.Body(), &resultBody)
	} else {
		log.Info("Trace info:", resp.Request.TraceInfo())
		return nil, errors.Wrap(err, fmt.Sprintf("Get request failed : URL - %v", endpointURL))
	}
	return resultBody, err
}
func DeleteRequestToMetamorph(endpointURL string) (map[string]interface{}, error) {
	resultBody := make(map[string]interface{})
	restyClient := resty.New()

	resp, err := restyClient.R().EnableTrace().Delete(endpointURL)

	if (err == nil) && (resp.StatusCode() == http.StatusOK) {
		err = json.Unmarshal(resp.Body(), &resultBody)
	} else {
		log.Info("Trace info:", resp.Request.TraceInfo())
		return nil, errors.Wrap(err, fmt.Sprintf("Delete request failed : URL - %v", endpointURL))
	}
	return resultBody, err
}

func (p *redfishProvisioner) retrieveNodeUUIDUsingRedfish() (string, error) {
	var nodeUUIDFromNode string

	//collect the UUID of the node for comparision with the value within DB
	nodeIPAddress := strings.Split(p.bmcAccess.DriverInfo(p.bmcCreds)["redfish_address"].(string), "/")
	log.Info("Node IPMI IP address retrieved ", nodeIPAddress[2])
	userName := p.bmcCreds.Username
	password := p.bmcCreds.Password

	hostInfo := "{ \"IPMIIP\" : \"%v\"," +
		"\"IPMIUser\" :  \"%v\"," +
		"\"IPMIPassword\" : \"%v\" }"

	hostInfo = fmt.Sprintf(hostInfo, nodeIPAddress[2], userName, password)

	resp, err := PostRequestToMetamorph(metamorphEndpoint+"uuid", []byte(hostInfo))

	if err != nil { //Issue with Redfish Check. No pint continuing.
		return "", err
	}

	nodeUUIDFromNode = resp["result"].(string)

	return nodeUUIDFromNode, nil
}

func (p *redfishProvisioner) findExistingHost() (result map[string]interface{}, err error) {
	//result = make(map[string]interface{})
	//Check if the Node UUID is currently known to metamorph.
	if p.status.ID != "" {

		//Check if it is present in the DB
		result, err = GetRequestToMetamorph(metamorphEndpoint + fmt.Sprintf("node/%v", p.status.ID))
		if err != nil {
			if strings.Contains(err.Error(), "Node not found") { // node was not found in DB and it is fine.
				err = nil
			}
			return nil, err
		}
	}
	return result, nil
}

// ValidateManagementAccess tests the connection information for
// the host to verify that the location and credentials work. The
// boolean argument tells the provisioner whether the current set
// of credentials it has are different from the credentials it has
// previously been using, without implying that either set of
// credentials is correct.
func (p *redfishProvisioner) ValidateManagementAccess(credentialsChanged bool) (result provisioner.Result, err error) {

	//check if the node is already registered
	node, err := p.findExistingHost()

	if err != nil {
		return result, errors.Wrap(err, "failed to find existing host")
	}

	if node == nil {
		nodeUUID, err := p.createNodeInMetamorph()
		if err != nil {
			return result, errors.Wrap(err, "failed to register host in redfish")
		}
		p.publisher("Registered", "Registered new host")
		p.status.ID = nodeUUID
		result.Dirty = true
		p.log.Info("setting provisioning id :ID", p.status.ID)

	} else {
		nodeUUID := node["NodeUUID"].(string)
		if p.status.ID != nodeUUID {
			// Store the ID so other methods can assume it is set and
			// so we can find the node using that value next time.
			p.status.ID = nodeUUID
			result.Dirty = true
			p.log.Info("setting provisioning id : ID", p.status.ID)
		}

	}
	return result, nil
}

// InspectHardware updates the HardwareDetails field of the host with
// details of devices discovered on the hardware. It may be called
// multiple times, and should return true for its dirty flag until the
// inspection is completed.
func (p *redfishProvisioner) InspectHardware() (result provisioner.Result, details *metal3v1alpha1.HardwareDetails, err error) {
	//Could be used to check if the curent node is as per the RAID/NIC/OS etc listed in the input
	//TODO : Add validation code to retreievethe computer SYstem info from IDRAC and fill it.
	// Currently dummy data added.
	details = new(metal3v1alpha1.HardwareDetails)
	if p.host.Status.HardwareDetails == nil {
		p.log.Info("continuing inspection by setting details")
		details =
			&metal3v1alpha1.HardwareDetails{
				RAMMebibytes: 128 * 1024,
				NIC: []metal3v1alpha1.NIC{
					metal3v1alpha1.NIC{
						Name:      "nic-1",
						Model:     "virt-io",
						MAC:       "ab:ba:ab:ba:ab:ba",
						IP:        "192.168.100.1",
						SpeedGbps: 1,
						PXE:       true,
					},
					metal3v1alpha1.NIC{
						Name:      "nic-2",
						Model:     "e1000",
						MAC:       "ab:ba:ab:ba:ab:bc",
						IP:        "192.168.100.2",
						SpeedGbps: 1,
						PXE:       false,
					},
				},
				Storage: []metal3v1alpha1.Storage{
					metal3v1alpha1.Storage{
						Name:       "disk-1 (boot)",
						Rotational: false,
						SizeBytes:  metal3v1alpha1.TebiByte * 93,
						Model:      "Dell CFJ61",
					},
					metal3v1alpha1.Storage{
						Name:       "disk-2",
						Rotational: false,
						SizeBytes:  metal3v1alpha1.TebiByte * 93,
						Model:      "Dell CFJ61",
					},
				},
				CPU: metal3v1alpha1.CPU{
					Arch:           "x86_64",
					Model:          "Core 2 Duo",
					ClockMegahertz: 3.0 * metal3v1alpha1.GigaHertz,
					Flags:          []string{"lm", "hypervisor", "vmx"},
					Count:          1,
				},
			}
		p.publisher("InspectionComplete", "Hardware inspection completed")
		p.host.SetOperationalStatus(metal3v1alpha1.OperationalStatusOK)
	}
	return
}

// UpdateHardwareState fetches the latest hardware state of the
// server and updates the HardwareDetails field of the host with
// details. It is expected to do this in the least expensive way
// possible, such as reading from a cache, and return dirty only
// if any state information has changed.InspectHardware
func (p *redfishProvisioner) UpdateHardwareState() (result provisioner.Result, err error) {

	p.log.Info("Updating Hardware State")

	node, err := p.findExistingHost()

	if err != nil {
		return result, errors.Wrap(err, "failed to find existing host")
	}
	if node == nil {
		return result, fmt.Errorf("No Redfish Node found for this host")
	}

	resp, err := GetRequestToMetamorph(metamorphEndpoint + fmt.Sprintf("hwstatus/%v", p.status.ID))
	if err != nil {
		return result, err
	}
	powerState := resp["result"].(string)

	var discoveredState bool

	switch powerState {
	case "On":
		discoveredState = true
	case "Off":
		discoveredState = false
	default:
		p.log.Info("Unknown power state : ", powerState)
		return result, nil
	}
	if discoveredState != p.host.Status.PoweredOn {
		p.log.Info("Updating Power Status to ", discoveredState)
		p.host.Status.PoweredOn = discoveredState
		result.RequeueAfter = powerRequeueDelay
		result.Dirty = true
	}

	return result, nil
}

// Adopt brings an externally-provisioned host under management by
// the provisioner.
func (p *redfishProvisioner) Adopt() (result provisioner.Result, err error) {
	node := make(map[string]interface{})
	if node, err = p.findExistingHost(); err != nil {
		err = errors.Wrap(err, "could not find host to adopt")
		return
	}
	if node == nil {
		p.log.Info("Re-registering host")
		return p.ValidateManagementAccess(true)
	}

	nodeState := node["State"].(string)

	//TBD : Add all the relevant state handling
	switch nodeState {
	case "failed":
		err = fmt.Errorf("Invalid state for adoption: %s", nodeState)
	default:
	}
	return
}

// Provision writes the image from the host spec to the host. It
// may be called multiple times, and should return true for its
// dirty flag until the deprovisioning operation is completed.
func (p *redfishProvisioner) Provision(hostconfigData provisioner.HostConfigData) (result provisioner.Result, err error) {

	node := make(map[string]interface{})

	if node, err = p.findExistingHost(); err != nil {
		err = errors.Wrap(err, "could not find host")
		return
	}
	if node == nil {
		return result, fmt.Errorf("No Redfish node for host found")
	}

	nodestate := node["State"].(string)

	switch nodestate {
	case "new":
		p.log.Info("Node in  state ", p.status.ID, nodestate)
		result.RequeueAfter = provisionRequeueDelay
		result.Dirty = true
		return
	case "in-transition":
		p.log.Info("Node in  state ", p.status.ID, nodestate)
		result.RequeueAfter = provisionRequeueDelay
		result.Dirty = true
		return
	case "readywait":
		p.log.Info("Node in  state ", p.status.ID, nodestate)

		// Collect the user data from the secrets and update the Node
		userdata, err := hostconfigData.UserData()
		if err != nil {
			return result, errors.Wrap(err, "could not retrieve user data")
		}

		p.log.Info(userdata)
		err = CheckPutRequestToMetamorph(p.status.ID, []byte(userdata))
		if err != nil {
			return result, errors.Wrap(err, "failed to Update  user data")
		}
		result.Dirty = true
		result.RequeueAfter = provisionRequeueDelay
	case "userdataloaded":
		p.log.Info("Node in  state ", p.status.ID, nodestate)

		//Make it state "Ready"
		stateData := []byte("{ \"State\" : \"ready\" }")
		err = CheckPutRequestToMetamorph(p.status.ID, stateData)
		if err != nil {
			return result, errors.Wrap(err, "failed to set node to READY state")
		}
		result.Dirty = true
		result.RequeueAfter = provisionRequeueDelay
	case "ready":
		result.Dirty = true
		result.RequeueAfter = provisionRequeueDelay
	case "setupreadywait":
		p.log.Info("Node in  state ", p.status.ID, nodestate)
		//Make it state "setupready"
		stateData := []byte("{ \"State\" : \"setupready\" }")
		err = CheckPutRequestToMetamorph(p.status.ID, stateData)
		if err != nil {
			return result, errors.Wrap(err, "failed to set node to SETUPREADY state")
		}
		result.Dirty = true
		result.RequeueAfter = provisionRequeueDelay

	case "setupready":
		p.log.Info("Node in  state ", p.status.ID, nodestate)

		result.RequeueAfter = provisionRequeueDelay
		result.Dirty = true
	case "deploying":
		p.log.Info("Node in  state ", p.status.ID, nodestate)

		result.RequeueAfter = provisionRequeueDelay
		result.Dirty = true
	case "deployed":
		p.log.Info("Node in  state ", p.status.ID, nodestate)
		return
	default:
		p.log.Info("Node in  state ", p.status.ID, nodestate)
		err = errors.Wrap(err, "Node in failed state")
	}

	return result, err
}

// Deprovision removes the host from the image. It may be called
// multiple times, and should return true for its dirty flag until
// the deprovisioning operation is completed.
func (p *redfishProvisioner) Deprovision() (result provisioner.Result, err error) {
	node := make(map[string]interface{})

	if node, err = p.findExistingHost(); err != nil {
		err = errors.Wrap(err, "could not find host")
		return
	}
	if node == nil {
		return result, fmt.Errorf("No Redfish node for host found")
	}

	p.publisher("DeprovisioningComplete", "Image deprovisioning completed")

	return result, nil
}

// Delete removes the host from the provisioning system. It may be
// called multiple times, and should return true for its dirty
// flag until the deprovisioning operation is completed.
func (p *redfishProvisioner) Delete() (result provisioner.Result, err error) {
	node := make(map[string]interface{})

	if node, err = p.findExistingHost(); err != nil {
		err = errors.Wrap(err, "could not find host")
		return
	}
	if node == nil {
		return result, fmt.Errorf("No Redfish node for host found")
	}

	//What are the states to be handled.
	resp, err := DeleteRequestToMetamorph(metamorphEndpoint + p.status.ID)
	if err != nil {
		return result, err
	}

	if resp["result"].(string) != "Successful" {
		return result, errors.Wrap(err, fmt.Sprintf("Failed to successfuly delete node %v", p.status.ID))
	}
	return result, nil

}

// PowerOn ensures the server is powered on independently of any image
// provisioning operation.
func (p *redfishProvisioner) PowerOn() (result provisioner.Result, err error) {
	node := make(map[string]interface{})

	if node, err = p.findExistingHost(); err != nil {
		err = errors.Wrap(err, "could not find host")
		return
	}
	if node == nil {
		return result, fmt.Errorf("No Redfish node for host found")
	}

	resp, err := GetRequestToMetamorph(metamorphEndpoint + fmt.Sprintf("hwstatus/%v", p.status.ID))
	if err != nil {
		return result, err
	}
	powerState := resp["result"].(string)
	if powerState != "Off" {
		reqdPowerState := "{ \"PowerState\" : \"On\" }"
		resp, err := PutRequestToMetamorph(metamorphEndpoint+fmt.Sprintf("hwstatus/%v", p.status.ID), []byte(reqdPowerState))
		if (err != nil) || (resp["result"].(string) != "Successful") {
			p.log.Info("Failed to change powerstate to ON")
			result.RequeueAfter = powerRequeueDelay
			return result, errors.Wrap(err, "failed to power on host")
		}
		p.publisher("PowerOn", "Host powered on")
	}

	return result, nil
}

// PowerOff ensures the server is powered off independently of any image
// provisioning operation.
func (p *redfishProvisioner) PowerOff() (result provisioner.Result, err error) {
	node := make(map[string]interface{})

	if node, err = p.findExistingHost(); err != nil {
		err = errors.Wrap(err, "could not find host")
		return
	}
	if node == nil {
		return result, fmt.Errorf("No Redfish node for host found")
	}
	resp, err := GetRequestToMetamorph(metamorphEndpoint + fmt.Sprintf("hwstatus/%v", p.status.ID))
	if err != nil {
		return result, err
	}
	powerState := resp["result"].(string)
	if powerState != "On" {
		reqdPowerState := "{ \"PowerState\" : \"Off\" }"
		resp, err := PutRequestToMetamorph(metamorphEndpoint+fmt.Sprintf("hwstatus/%v", p.status.ID), []byte(reqdPowerState))
		if (err != nil) || (resp["result"].(string) != "Successful") {
			p.log.Info("Failed to change powerstate to OFF")
			result.RequeueAfter = powerRequeueDelay
			return result, errors.Wrap(err, "failed to power off host")
		}
		p.publisher("PowerOff", "Host powered off")
	}
	return result, nil
}
