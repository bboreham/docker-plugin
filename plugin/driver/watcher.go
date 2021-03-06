package driver

import (
	"fmt"
	"strings"

	"github.com/fsouza/go-dockerclient"
	. "github.com/weaveworks/weave/common"
)

const (
	WeaveDNSContainer = "weavedns"
	WeaveDomain       = "weave.local"
)

type watcher struct {
	dockerer
	networks map[string]bool
	events   chan *docker.APIEvents
}

type Watcher interface {
	WatchNetwork(uuid string)
	UnwatchNetwork(uuid string)
}

func NewWatcher(client *docker.Client) (Watcher, error) {
	w := &watcher{
		dockerer: dockerer{
			client: client,
		},
		networks: make(map[string]bool),
		events:   make(chan *docker.APIEvents),
	}
	err := client.AddEventListener(w.events)
	if err != nil {
		return nil, err
	}

	go func() {
		for event := range w.events {
			switch event.Status {
			case "start":
				w.ContainerStart(event.ID)
			case "die":
				w.ContainerDied(event.ID)
			}
		}
	}()

	return w, nil
}

func (w *watcher) WatchNetwork(uuid string) {
	Debug.Printf("Watch network %s", uuid)
	w.networks[uuid] = true
}

func (w *watcher) UnwatchNetwork(uuid string) {
	Debug.Printf("Unwatch network %s", uuid)
	delete(w.networks, uuid)
}

func (w *watcher) ContainerStart(id string) {
	Debug.Printf("Container started %s", id)
	info, err := w.InspectContainer(id)
	if err != nil {
		Warning.Printf("error inspecting container: %s", err)
		return
	}
	// FIXME: check that it's on our network; but, the docker client lib doesn't know about .NetworkID
	if isSubdomain(info.Config.Domainname, WeaveDomain) {
		// one of ours
		ip := info.NetworkSettings.IPAddress
		fqdn := fmt.Sprintf("%s.%s", info.Config.Hostname, info.Config.Domainname)
		if err := w.registerWithDNS(id, fqdn, ip); err != nil {
			Warning.Printf("unable to register with weaveDNS: %s", err)
		}
	}
}

func (w *watcher) ContainerDied(id string) {
	Debug.Printf("Container died %s", id)
	info, err := w.InspectContainer(id)
	if err != nil {
		Warning.Printf("error inspecting container: %s", err)
		return
	}
	if isSubdomain(info.Config.Domainname, WeaveDomain) {
		ip := info.NetworkSettings.IPAddress
		if err := w.deregisterWithDNS(id, ip); err != nil {
			Warning.Printf("unable to deregister with weaveDNS: %s", err)
		}
	}
}

// Cheap and cheerful way to check x is, or is a subdomain, of
// y. Neither are expected to start with a '.'.
func isSubdomain(x string, y string) bool {
	return x == y || strings.HasSuffix(x, "."+y)
}
