package main

import (
	"errors"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
	"net"
)

/* Describes the creation of binds and endpoints on the platform
 */
type Networking interface {
	CreateBind(port uint16) (Bind, uint16, error)
	CreateEndpoint(addr string) (Endpoint, error)
}

/* A Bind handles listening on a port for both IPv6 and IPv4 UDP traffic
 * - Similar to a net.UDPConn interface.
 */
type Bind interface {
	SetMark(value uint32) error
	ReceiveIPv6(buff []byte) (int, Endpoint, error)
	ReceiveIPv4(buff []byte) (int, Endpoint, error)
	Send(buff []byte, end Endpoint) error
	Close() error
}

/* An Endpoint maintains the source/destination caching for a peer
 *
 * dst : the remote address of a peer ("endpoint" in uapi terminology)
 * src : the local address from which datagrams originate going to the peer
 */
type Endpoint interface {
	ClearSrc()           // clears the source address
	SrcToString() string // returns the local source address (ip:port)
	DstToString() string // returns the destination address (ip:port)
	DstToBytes() []byte  // used for mac2 cookie calculations
	DstIP() net.IP
	SrcIP() net.IP
}

func parseEndpoint(s string) (*net.UDPAddr, error) {

	// ensure that the host is an IP address (e.g. not a domain name)

	host, _, err := net.SplitHostPort(s)
	if err != nil {
		return nil, err
	}
	if ip := net.ParseIP(host); ip == nil {
		return nil, errors.New("Failed to parse IP address: " + host)
	}

	// parse address and port

	addr, err := net.ResolveUDPAddr("udp", s)
	if err != nil {
		return nil, err
	}
	return addr, err
}

/* Must hold device and net lock
 */
func unsafeCloseBind(device *Device) error {
	var err error
	netc := &device.net
	if netc.bind != nil {
		err = netc.bind.Close()
		netc.bind = nil
	}
	return err
}

func (device *Device) BindSetMark(mark uint32) error {

	device.net.mutex.Lock()
	defer device.net.mutex.Unlock()

	device.peers.mutex.Lock()
	defer device.peers.mutex.Unlock()

	// check if modified

	if device.net.fwmark == mark {
		return nil
	}

	// update fwmark on existing bind

	device.net.fwmark = mark
	if device.isUp.Get() && device.net.bind != nil {
		if err := device.net.bind.SetMark(mark); err != nil {
			return err
		}
	}

	return nil
}

func (device *Device) BindUpdate() error {

	device.net.mutex.Lock()
	defer device.net.mutex.Unlock()

	device.peers.mutex.Lock()
	defer device.peers.mutex.Unlock()

	// close existing sockets

	if err := unsafeCloseBind(device); err != nil {
		return err
	}

	// open new sockets

	if device.isUp.Get() {

		// bind to new port

		var err error
		netc := &device.net
		netc.bind, netc.port, err = device.net.network.CreateBind(netc.port)
		if err != nil {
			netc.bind = nil
			netc.port = 0
			return err
		}

		// set fwmark

		if netc.fwmark != 0 {
			err = netc.bind.SetMark(netc.fwmark)
			if err != nil {
				return err
			}
		}

		// clear cached source addresses

		for _, peer := range device.peers.keyMap {
			peer.mutex.Lock()
			defer peer.mutex.Unlock()
			if peer.endpoint != nil {
				peer.endpoint.ClearSrc()
			}
		}

		// start receiving routines

		go device.RoutineReceiveIncoming(ipv4.Version, netc.bind)
		go device.RoutineReceiveIncoming(ipv6.Version, netc.bind)

		device.log.Debug.Println("UDP bind has been updated")
	}

	return nil
}

func (device *Device) BindClose() error {
	device.net.mutex.Lock()
	err := unsafeCloseBind(device)
	device.net.mutex.Unlock()
	return err
}
