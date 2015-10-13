package driver

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"

	"github.com/docker/libnetwork/drivers/remote/api"
	"github.com/docker/libnetwork/types"

	. "github.com/weaveworks/weave/common"

	docker "github.com/fsouza/go-dockerclient"
	"github.com/gorilla/mux"
	"github.com/vishvananda/netlink"
)

const (
	MethodReceiver = "NetworkDriver"
	WeaveContainer = "weave"
	WeaveBridge    = "weave"
)

type Driver interface {
	SetNameserver(string) error
	Listen(net.Listener) error
}

type driver struct {
	dockerer
	version    string
	network    string
	nameserver string
	watcher    Watcher
}

func New(version string) (Driver, error) {
	client, err := docker.NewClient("unix:///var/run/docker.sock")
	if err != nil {
		return nil, fmt.Errorf("could not connect to docker: %s", err)
	}

	watcher, err := NewWatcher(client)
	if err != nil {
		return nil, err
	}

	return &driver{
		dockerer: dockerer{
			client: client,
		},
		version: version,
		watcher: watcher,
	}, nil
}

func (driver *driver) SetNameserver(nameserver string) error {
	if net.ParseIP(nameserver) == nil {
		return fmt.Errorf(`cannot parse IP address "%s"`, nameserver)
	}
	driver.nameserver = nameserver
	return nil
}

func (driver *driver) Listen(socket net.Listener) error {
	router := mux.NewRouter()
	router.NotFoundHandler = http.HandlerFunc(notFound)

	router.Methods("GET").Path("/status").HandlerFunc(driver.status)
	router.Methods("POST").Path("/Plugin.Activate").HandlerFunc(driver.handshake)

	handleMethod := func(method string, h http.HandlerFunc) {
		router.Methods("POST").Path(fmt.Sprintf("/%s.%s", MethodReceiver, method)).HandlerFunc(h)
	}

	handleMethod("GetCapabilities", driver.getCapabilities)
	handleMethod("CreateNetwork", driver.createNetwork)
	handleMethod("DeleteNetwork", driver.deleteNetwork)
	handleMethod("CreateEndpoint", driver.createEndpoint)
	handleMethod("DeleteEndpoint", driver.deleteEndpoint)
	handleMethod("EndpointOperInfo", driver.infoEndpoint)
	handleMethod("Join", driver.joinEndpoint)
	handleMethod("Leave", driver.leaveEndpoint)

	return http.Serve(socket, router)
}

func notFound(w http.ResponseWriter, r *http.Request) {
	Log.Warningf("[plugin] Not found: %+v", r)
	http.NotFound(w, r)
}

func sendError(w http.ResponseWriter, msg string, code int) {
	Log.Errorf("%d %s", code, msg)
	http.Error(w, msg, code)
}

func errorResponsef(w http.ResponseWriter, fmtString string, item ...interface{}) {
	json.NewEncoder(w).Encode(map[string]string{
		"Err": fmt.Sprintf(fmtString, item...),
	})
}

func objectResponse(w http.ResponseWriter, obj interface{}) {
	if err := json.NewEncoder(w).Encode(obj); err != nil {
		sendError(w, "Could not JSON encode response", http.StatusInternalServerError)
		return
	}
}

func emptyResponse(w http.ResponseWriter) {
	json.NewEncoder(w).Encode(map[string]string{})
}

// === protocol handlers

type handshakeResp struct {
	Implements []string
}

func (driver *driver) handshake(w http.ResponseWriter, r *http.Request) {
	err := json.NewEncoder(w).Encode(&handshakeResp{
		[]string{"NetworkDriver"},
	})
	if err != nil {
		sendError(w, "encode error", http.StatusInternalServerError)
		Log.Error("handshake encode:", err)
		return
	}
	Log.Infof("Handshake completed")
}

func (driver *driver) status(w http.ResponseWriter, r *http.Request) {
	io.WriteString(w, fmt.Sprintln("weave plugin", driver.version))
}

var caps = &api.GetCapabilityResponse{
	Scope: "global",
}

func (driver *driver) getCapabilities(w http.ResponseWriter, r *http.Request) {
	objectResponse(w, caps)
	Log.Debugf("Get capabilities: responded with %+v", caps)
}

func (driver *driver) createNetwork(w http.ResponseWriter, r *http.Request) {
	var create api.CreateNetworkRequest
	err := json.NewDecoder(r.Body).Decode(&create)
	if err != nil {
		sendError(w, "Unable to decode JSON payload: "+err.Error(), http.StatusBadRequest)
		return
	}
	Log.Debugf("Create network request %+v", &create)

	if driver.network != "" {
		errorResponsef(w, "You get just one network, and you already made %s", driver.network)
		return
	}

	driver.network = create.NetworkID
	driver.watcher.WatchNetwork(driver.network)
	emptyResponse(w)
	Log.Infof("Create network %s", driver.network)
}

func (driver *driver) deleteNetwork(w http.ResponseWriter, r *http.Request) {
	var delete api.DeleteNetworkRequest
	if err := json.NewDecoder(r.Body).Decode(&delete); err != nil {
		sendError(w, "Unable to decode JSON payload: "+err.Error(), http.StatusBadRequest)
		return
	}
	Log.Debugf("Delete network request: %+v", &delete)
	if delete.NetworkID != driver.network {
		errorResponsef(w, "Network %s not found", delete.NetworkID)
		return
	}
	driver.network = ""
	driver.watcher.UnwatchNetwork(delete.NetworkID)
	emptyResponse(w)
	Log.Infof("Destroy network %s", delete.NetworkID)
}

func (driver *driver) createEndpoint(w http.ResponseWriter, r *http.Request) {
	var create api.CreateEndpointRequest
	if err := json.NewDecoder(r.Body).Decode(&create); err != nil {
		sendError(w, "Unable to decode JSON payload: "+err.Error(), http.StatusBadRequest)
		return
	}
	Log.Debugf("Create endpoint request %+v", &create)
	netID := create.NetworkID
	endID := create.EndpointID

	if netID != driver.network {
		errorResponsef(w, "No such network %s", netID)
		return
	}

	ip, err := driver.allocateIP(endID)
	if err != nil {
		Log.Warningf("Error allocating IP: %s", err)
		sendError(w, "Unable to allocate IP", http.StatusInternalServerError)
		return
	}
	Log.Debugf("Got IP from IPAM %s", ip.String())

	mac := makeMac(ip.IP)

	respIface := &api.EndpointInterface{
		Address:    ip.String(),
		MacAddress: mac,
	}
	resp := &api.CreateEndpointResponse{
		Interface: respIface,
	}

	objectResponse(w, resp)
	Log.Infof("Create endpoint %s %+v", endID, resp)
}

func (driver *driver) deleteEndpoint(w http.ResponseWriter, r *http.Request) {
	var delete api.DeleteEndpointRequest
	if err := json.NewDecoder(r.Body).Decode(&delete); err != nil {
		sendError(w, "Could not decode JSON encode payload", http.StatusBadRequest)
		return
	}
	Log.Debugf("Delete endpoint request: %+v", &delete)
	emptyResponse(w)
	if err := driver.releaseIP(delete.EndpointID); err != nil {
		Log.Warningf("error releasing IP: %s", err)
	}
	Log.Infof("Delete endpoint %s", delete.EndpointID)
}

func (driver *driver) infoEndpoint(w http.ResponseWriter, r *http.Request) {
	var info api.EndpointInfoRequest
	if err := json.NewDecoder(r.Body).Decode(&info); err != nil {
		sendError(w, "Could not decode JSON encode payload", http.StatusBadRequest)
		return
	}
	Log.Debugf("Endpoint info request: %+v", &info)
	objectResponse(w, &api.EndpointInfoResponse{Value: map[string]interface{}{}})
	Log.Infof("Endpoint info %s", info.EndpointID)
}

func (driver *driver) joinEndpoint(w http.ResponseWriter, r *http.Request) {
	var j api.JoinRequest
	if err := json.NewDecoder(r.Body).Decode(&j); err != nil {
		sendError(w, "Could not decode JSON encode payload", http.StatusBadRequest)
		return
	}
	Log.Debugf("Join request: %+v", &j)

	endID := j.EndpointID

	// create and attach local name to the bridge
	local := vethPair(endID[:5])
	if err := netlink.LinkAdd(local); err != nil {
		Log.Error(err)
		errorResponsef(w, "could not create veth pair")
		return
	}

	var bridge *netlink.Bridge
	if maybeBridge, err := netlink.LinkByName(WeaveBridge); err != nil {
		Log.Error(err)
		errorResponsef(w, `bridge "%s" not present`, WeaveBridge)
		return
	} else {
		var ok bool
		if bridge, ok = maybeBridge.(*netlink.Bridge); !ok {
			Log.Errorf("%s is %+v", WeaveBridge, maybeBridge)
			errorResponsef(w, `device "%s" not a bridge`, WeaveBridge)
			return
		}
	}
	if netlink.LinkSetMaster(local, bridge) != nil || netlink.LinkSetUp(local) != nil {
		errorResponsef(w, `unable to bring veth up`)
		return
	}

	ifname := &api.InterfaceName{
		SrcName:   local.PeerName,
		DstPrefix: "ethwe",
	}

	res := &api.JoinResponse{
		InterfaceName: ifname,
	}
	if driver.nameserver != "" {
		routeToDNS := api.StaticRoute{
			Destination: driver.nameserver + "/32",
			RouteType:   types.CONNECTED,
			NextHop:     "",
		}
		res.StaticRoutes = []api.StaticRoute{routeToDNS}
	}

	objectResponse(w, res)
	Log.Infof("Join endpoint %s:%s to %s", j.NetworkID, j.EndpointID, j.SandboxKey)
}

func (driver *driver) leaveEndpoint(w http.ResponseWriter, r *http.Request) {
	var l api.LeaveRequest
	if err := json.NewDecoder(r.Body).Decode(&l); err != nil {
		sendError(w, "Could not decode JSON encode payload", http.StatusBadRequest)
		return
	}
	Log.Debugf("Leave request: %+v", &l)

	local := vethPair(l.EndpointID[:5])
	if err := netlink.LinkDel(local); err != nil {
		Log.Warningf("unable to delete veth on leave: %s", err)
	}
	emptyResponse(w)
	Log.Infof("Leave %s:%s", l.NetworkID, l.EndpointID)
}

// ===

func vethPair(suffix string) *netlink.Veth {
	return &netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{Name: "vethwl" + suffix},
		PeerName:  "vethwg" + suffix,
	}
}

func makeMac(ip net.IP) string {
	hw := make(net.HardwareAddr, 6)
	hw[0] = 0x7a
	hw[1] = 0x42
	copy(hw[2:], ip.To4())
	return hw.String()
}
