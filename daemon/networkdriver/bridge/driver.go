package bridge

import (
	"fmt"
	"io/ioutil"
	"net"
	"strings"
	"sync"

	"github.com/docker/docker/daemon/networkdriver"
	"github.com/docker/docker/daemon/networkdriver/ipallocator"
	"github.com/docker/docker/daemon/networkdriver/portallocator"
	"github.com/docker/docker/daemon/networkdriver/portmapper"
	"github.com/docker/docker/engine"
	"github.com/docker/docker/pkg/iptables"
	"github.com/docker/docker/pkg/log"
	"github.com/docker/docker/pkg/networkfs/resolvconf"
	"github.com/docker/docker/pkg/parsers/kernel"
	"github.com/docker/libcontainer/netlink"
)

const (
	DefaultNetworkBridge     = "docker0"
	MaxAllocatedPortAttempts = 10
)

// Network interface represents the networking stack of a container
type networkInterface struct {
	IP           net.IP
	PortMappings []net.Addr // there are mappings to the host interfaces
}

type ifaces struct {
	c map[string]*networkInterface
	sync.Mutex
}

func (i *ifaces) Set(key string, n *networkInterface) {
	i.Lock()
	i.c[key] = n
	i.Unlock()
}

func (i *ifaces) Get(key string) *networkInterface {
	i.Lock()
	res := i.c[key]
	i.Unlock()
	return res
}

var (
	addrs = []string{
		// Here we don't follow the convention of using the 1st IP of the range for the gateway.
		// This is to use the same gateway IPs as the /24 ranges, which predate the /16 ranges.
		// In theory this shouldn't matter - in practice there's bound to be a few scripts relying
		// on the internal addressing or other stupid things like that.
		// They shouldn't, but hey, let's not break them unless we really have to.
		"172.17.42.1/16", // Don't use 172.16.0.0/16, it conflicts with EC2 DNS 172.16.0.23
		"10.0.42.1/16",   // Don't even try using the entire /8, that's too intrusive
		"10.1.42.1/16",
		"10.42.42.1/16",
		"172.16.42.1/24",
		"172.16.43.1/24",
		"172.16.44.1/24",
		"10.0.42.1/24",
		"10.0.43.1/24",
		"192.168.42.1/24",
		"192.168.43.1/24",
		"192.168.44.1/24",
	}

	bridgeIface   string
	bridgeNetwork *net.IPNet

	defaultBindingIP  = net.ParseIP("0.0.0.0")
	currentInterfaces = ifaces{c: make(map[string]*networkInterface)}
)

func InitDriver(job *engine.Job) engine.Status {
	var (
		network        *net.IPNet
		enableIPTables = job.GetenvBool("EnableIptables")
		icc            = job.GetenvBool("InterContainerCommunication")
		ipForward      = job.GetenvBool("EnableIpForward")
		bridgeIP       = job.Getenv("BridgeIP")
	)

	if defaultIP := job.Getenv("DefaultBindingIP"); defaultIP != "" {
		defaultBindingIP = net.ParseIP(defaultIP)
	}

	bridgeIface = job.Getenv("BridgeIface")
	usingDefaultBridge := false
	if bridgeIface == "" {
		usingDefaultBridge = true
		bridgeIface = DefaultNetworkBridge
	}
	//查找该设备是否存在 如果网桥设备存在就返回ip地址
	addr, err := networkdriver.GetIfaceAddr(bridgeIface)
	if err != nil {
		// If we're not using the default bridge, fail without trying to create it
		if !usingDefaultBridge {
			job.Logf("bridge not found: %s", bridgeIface)
			return job.Error(err)
		}
		// If the iface is not found, try to create it
		job.Logf("creating new bridge for %s", bridgeIface) //创建docker0网桥
		if err := createBridge(bridgeIP); err != nil {
			return job.Error(err)
		}

		job.Logf("getting iface addr")
		addr, err = networkdriver.GetIfaceAddr(bridgeIface)
		if err != nil {
			return job.Error(err)
		}
		network = addr.(*net.IPNet)
	} else {
		network = addr.(*net.IPNet)
		// validate that the bridge ip matches the ip specified by BridgeIP
		if bridgeIP != "" {
			bip, _, err := net.ParseCIDR(bridgeIP)
			if err != nil {
				return job.Error(err)
			}
			if !network.IP.Equal(bip) {
				return job.Errorf("bridge ip (%s) does not match existing bridge configuration %s", network.IP, bip)
			}
		}
	}

	// Configure iptables for link support
	if enableIPTables {
		if err := setupIPTables(addr, icc); err != nil {
			return job.Error(err)
		}
	}

	if ipForward {
		// Enable IPv4 forwarding
		if err := ioutil.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte{'1', '\n'}, 0644); err != nil {
			job.Logf("WARNING: unable to enable IPv4 forwarding: %s\n", err)
		}
	}

	// We can always try removing the iptables
	if err := iptables.RemoveExistingChain("DOCKER"); err != nil {
		return job.Error(err)
	}

	if enableIPTables {
		chain, err := iptables.NewChain("DOCKER", bridgeIface)
		if err != nil {
			return job.Error(err)
		}
		portmapper.SetIptablesChain(chain)
	}

	bridgeNetwork = network

	// https://github.com/docker/docker/issues/2768
	job.Eng.Hack_SetGlobalVar("httpapi.bridgeIP", bridgeNetwork.IP)

	for name, f := range map[string]engine.Handler{
		"allocate_interface": Allocate,
		"release_interface":  Release,
		"allocate_port":      AllocatePort,
		"link":               LinkContainers,
	} {
		if err := job.Eng.Register(name, f); err != nil {
			return job.Error(err)
		}
	}
	return engine.StatusOK
}

func setupIPTables(addr net.Addr, icc bool) error { //icc决定docker容器之间是否通信
	// Enable NAT
	natArgs := []string{"POSTROUTING", "-t", "nat", "-s", addr.String(), "!", "-o", bridgeIface, "-j", "MASQUERADE"}

	if !iptables.Exists(natArgs...) {
		if output, err := iptables.Raw(append([]string{"-I"}, natArgs...)...); err != nil {
			return fmt.Errorf("Unable to enable network bridge NAT: %s", err)
		} else if len(output) != 0 {
			return fmt.Errorf("Error iptables postrouting: %s", output)
		}
	}

	var (
		args       = []string{"FORWARD", "-i", bridgeIface, "-o", bridgeIface, "-j"}
		acceptArgs = append(args, "ACCEPT")
		dropArgs   = append(args, "DROP")
	)

	if !icc {
		iptables.Raw(append([]string{"-D"}, acceptArgs...)...)

		if !iptables.Exists(dropArgs...) {
			log.Debugf("Disable inter-container communication")
			if output, err := iptables.Raw(append([]string{"-I"}, dropArgs...)...); err != nil {
				return fmt.Errorf("Unable to prevent intercontainer communication: %s", err)
			} else if len(output) != 0 {
				return fmt.Errorf("Error disabling intercontainer communication: %s", output)
			}
		}
	} else {
		iptables.Raw(append([]string{"-D"}, dropArgs...)...)

		if !iptables.Exists(acceptArgs...) {
			log.Debugf("Enable inter-container communication")
			if output, err := iptables.Raw(append([]string{"-I"}, acceptArgs...)...); err != nil {
				return fmt.Errorf("Unable to allow intercontainer communication: %s", err)
			} else if len(output) != 0 {
				return fmt.Errorf("Error enabling intercontainer communication: %s", output)
			}
		}
	}

	// Accept all non-intercontainer outgoing packets
	outgoingArgs := []string{"FORWARD", "-i", bridgeIface, "!", "-o", bridgeIface, "-j", "ACCEPT"}
	if !iptables.Exists(outgoingArgs...) {
		if output, err := iptables.Raw(append([]string{"-I"}, outgoingArgs...)...); err != nil {
			return fmt.Errorf("Unable to allow outgoing packets: %s", err)
		} else if len(output) != 0 {
			return fmt.Errorf("Error iptables allow outgoing: %s", output)
		}
	}

	// Accept incoming packets for existing connections
	existingArgs := []string{"FORWARD", "-o", bridgeIface, "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT"}

	if !iptables.Exists(existingArgs...) {
		if output, err := iptables.Raw(append([]string{"-I"}, existingArgs...)...); err != nil {
			return fmt.Errorf("Unable to allow incoming packets: %s", err)
		} else if len(output) != 0 {
			return fmt.Errorf("Error iptables allow incoming: %s", output)
		}
	}
	return nil
}

// CreateBridgeIface creates a network bridge interface on the host system with the name `ifaceName`,
// and attempts to configure it with an address which doesn't conflict with any other interface on the host.
// If it can't find an address which doesn't conflict, it will return an error.
func createBridge(bridgeIP string) error {
	nameservers := []string{}
	resolvConf, _ := resolvconf.Get()
	// we don't check for an error here, because we don't really care
	// if we can't read /etc/resolv.conf. So instead we skip the append
	// if resolvConf is nil. It either doesn't exist, or we can't read it
	// for some reason.
	if resolvConf != nil {
		nameservers = append(nameservers, resolvconf.GetNameserversAsCIDR(resolvConf)...)
	}

	var ifaceAddr string
	if len(bridgeIP) != 0 {
		_, _, err := net.ParseCIDR(bridgeIP)
		if err != nil {
			return err
		}
		ifaceAddr = bridgeIP
	} else {
		for _, addr := range addrs {
			_, dockerNetwork, err := net.ParseCIDR(addr)
			if err != nil {
				return err
			}
			if err := networkdriver.CheckNameserverOverlaps(nameservers, dockerNetwork); err == nil {
				if err := networkdriver.CheckRouteOverlaps(dockerNetwork); err == nil {
					ifaceAddr = addr
					break
				} else {
					log.Debugf("%s %s", addr, err)
				}
			}
		}
	}

	if ifaceAddr == "" {
		return fmt.Errorf("Could not find a free IP address range for interface '%s'. Please configure its address manually and run 'docker -b %s'", bridgeIface, bridgeIface)
	}
	log.Debugf("Creating bridge %s with network %s", bridgeIface, ifaceAddr)

	if err := createBridgeIface(bridgeIface); err != nil {
		return err
	}

	iface, err := net.InterfaceByName(bridgeIface)
	if err != nil {
		return err
	}

	ipAddr, ipNet, err := net.ParseCIDR(ifaceAddr)
	if err != nil {
		return err
	}

	if netlink.NetworkLinkAddIp(iface, ipAddr, ipNet); err != nil {
		return fmt.Errorf("Unable to add private network: %s", err)
	}
	if err := netlink.NetworkLinkUp(iface); err != nil {
		return fmt.Errorf("Unable to start network bridge: %s", err)
	}
	return nil
}

func createBridgeIface(name string) error {
	kv, err := kernel.GetKernelVersion()
	// only set the bridge's mac address if the kernel version is > 3.3
	// before that it was not supported
	setBridgeMacAddr := err == nil && (kv.Kernel >= 3 && kv.Major >= 3)
	log.Debugf("setting bridge mac address = %v", setBridgeMacAddr)
	return netlink.CreateBridge(name, setBridgeMacAddr)
}

// Allocate a network interface
func Allocate(job *engine.Job) engine.Status {
	var (
		ip          *net.IP
		err         error
		id          = job.Args[0]
		requestedIP = net.ParseIP(job.Getenv("RequestedIP"))
	)

	if requestedIP != nil {
		ip, err = ipallocator.RequestIP(bridgeNetwork, &requestedIP)
	} else {
		ip, err = ipallocator.RequestIP(bridgeNetwork, nil)
	}
	if err != nil {
		return job.Error(err)
	}

	out := engine.Env{}
	out.Set("IP", ip.String())
	out.Set("Mask", bridgeNetwork.Mask.String())
	out.Set("Gateway", bridgeNetwork.IP.String())
	out.Set("Bridge", bridgeIface)

	size, _ := bridgeNetwork.Mask.Size()
	out.SetInt("IPPrefixLen", size)

	currentInterfaces.Set(id, &networkInterface{
		IP: *ip,
	})

	out.WriteTo(job.Stdout)

	return engine.StatusOK
}

// release an interface for a select ip
func Release(job *engine.Job) engine.Status {
	var (
		id                 = job.Args[0]
		containerInterface = currentInterfaces.Get(id)
	)

	if containerInterface == nil {
		return job.Errorf("No network information to release for %s", id)
	}

	for _, nat := range containerInterface.PortMappings {
		if err := portmapper.Unmap(nat); err != nil {
			log.Infof("Unable to unmap port %s: %s", nat, err)
		}
	}

	if err := ipallocator.ReleaseIP(bridgeNetwork, &containerInterface.IP); err != nil {
		log.Infof("Unable to release ip %s", err)
	}
	return engine.StatusOK
}

// Allocate an external port and map it to the interface
func AllocatePort(job *engine.Job) engine.Status {
	var (
		err error

		ip            = defaultBindingIP
		id            = job.Args[0]
		hostIP        = job.Getenv("HostIP")
		hostPort      = job.GetenvInt("HostPort")
		containerPort = job.GetenvInt("ContainerPort")
		proto         = job.Getenv("Proto")
		network       = currentInterfaces.Get(id)
	)

	if hostIP != "" {
		ip = net.ParseIP(hostIP)
	}

	// host ip, proto, and host port
	var container net.Addr
	switch proto {
	case "tcp":
		container = &net.TCPAddr{IP: network.IP, Port: containerPort}
	case "udp":
		container = &net.UDPAddr{IP: network.IP, Port: containerPort}
	default:
		return job.Errorf("unsupported address type %s", proto)
	}

	//
	// Try up to 10 times to get a port that's not already allocated.
	//
	// In the event of failure to bind, return the error that portmapper.Map
	// yields.
	//

	var host net.Addr
	for i := 0; i < MaxAllocatedPortAttempts; i++ {
		if host, err = portmapper.Map(container, ip, hostPort); err == nil {
			break
		}

		if allocerr, ok := err.(portallocator.ErrPortAlreadyAllocated); ok {
			// There is no point in immediately retrying to map an explicitly
			// chosen port.
			if hostPort != 0 {
				job.Logf("Failed to bind %s for container address %s: %s", allocerr.IPPort(), container.String(), allocerr.Error())
				break
			}

			// Automatically chosen 'free' port failed to bind: move on the next.
			job.Logf("Failed to bind %s for container address %s. Trying another port.", allocerr.IPPort(), container.String())
		} else {
			// some other error during mapping
			job.Logf("Received an unexpected error during port allocation: %s", err.Error())
			break
		}
	}

	if err != nil {
		return job.Error(err)
	}

	network.PortMappings = append(network.PortMappings, host)

	out := engine.Env{}
	switch netAddr := host.(type) {
	case *net.TCPAddr:
		out.Set("HostIP", netAddr.IP.String())
		out.SetInt("HostPort", netAddr.Port)
	case *net.UDPAddr:
		out.Set("HostIP", netAddr.IP.String())
		out.SetInt("HostPort", netAddr.Port)
	}
	if _, err := out.WriteTo(job.Stdout); err != nil {
		return job.Error(err)
	}

	return engine.StatusOK
}

func LinkContainers(job *engine.Job) engine.Status {
	var (
		action       = job.Args[0]
		childIP      = job.Getenv("ChildIP")
		parentIP     = job.Getenv("ParentIP")
		ignoreErrors = job.GetenvBool("IgnoreErrors")
		ports        = job.GetenvList("Ports")
	)
	split := func(p string) (string, string) {
		parts := strings.Split(p, "/")
		return parts[0], parts[1]
	}

	for _, p := range ports {
		port, proto := split(p)
		if output, err := iptables.Raw(action, "FORWARD",
			"-i", bridgeIface, "-o", bridgeIface,
			"-p", proto,
			"-s", parentIP,
			"--dport", port,
			"-d", childIP,
			"-j", "ACCEPT"); !ignoreErrors && err != nil {
			return job.Error(err)
		} else if len(output) != 0 {
			return job.Errorf("Error toggle iptables forward: %s", output)
		}

		if output, err := iptables.Raw(action, "FORWARD",
			"-i", bridgeIface, "-o", bridgeIface,
			"-p", proto,
			"-s", childIP,
			"--sport", port,
			"-d", parentIP,
			"-j", "ACCEPT"); !ignoreErrors && err != nil {
			return job.Error(err)
		} else if len(output) != 0 {
			return job.Errorf("Error toggle iptables forward: %s", output)
		}
	}
	return engine.StatusOK
}
