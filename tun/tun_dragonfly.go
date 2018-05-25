/* SPDX-License-Identifier: GPL-2.0
 *
 * Copyright (C) 2017-2018 Jason A. Donenfeld <Jason@zx2c4.com>. All Rights Reserved.
 */

package tun

import (
	"bytes"
	"errors"
	"fmt"
	"git.zx2c4.com/wireguard-go/rwcancel"
	"golang.org/x/net/ipv6"
	"golang.org/x/sys/unix"
	"net"
	"os"
	"syscall"
	"unsafe"
)

// _TUNSIFHEAD, value derived from sys/net/{if_tun,ioccom}.h
// const _TUNSIFHEAD = ((0x80000000) | (((4) & ((1 << 13) - 1) ) << 16) | (uint32(byte('t')) << 8) | (96))
const _TUNSIFHEAD = 0x80047460
const _TUNSIFMODE = 0x8004745e
const _TUNSIFPID = 0x2000745f

// Iface status string max len
const _IFSTATMAX = 800

const SIZEOF_UINTPTR = 4 << (^uintptr(0) >> 32 & 1)

// structure for iface requests with a pointer
type ifreq_ptr struct {
	Name [unix.IFNAMSIZ]byte
	Data uintptr
	Pad0 [24 - SIZEOF_UINTPTR]byte
}

// Structure for iface mtu get/set ioctls
type ifreq_mtu struct {
	Name [unix.IFNAMSIZ]byte
	MTU  uint32
	Pad0 [12]byte
}

// Structure for interface status request ioctl
type ifstat struct {
	IfsName [unix.IFNAMSIZ]byte
	Ascii   [_IFSTATMAX]byte
}

type nativeTun struct {
	name        string
	origName	string
	fd          *os.File
	rwcancel    *rwcancel.RWCancel
	events      chan TUNEvent
	errors      chan error
	routeSocket int
}

func (tun *nativeTun) routineRouteListener(tunIfindex int) {
	var (
		statusUp  bool
		statusMTU int
	)

	defer close(tun.events)

	data := make([]byte, os.Getpagesize())
	for {
	retry:
		n, err := unix.Read(tun.routeSocket, data)
		if err != nil {
			if errno, ok := err.(syscall.Errno); ok && errno == syscall.EINTR {
				goto retry
			}
			tun.errors <- err
			return
		}

		if n < 14 {
			continue
		}

		if data[3 /* type */] != unix.RTM_IFINFO {
			continue
		}
		ifindex := int(*(*uint16)(unsafe.Pointer(&data[12 /* ifindex */])))
		if ifindex != tunIfindex {
			continue
		}

		iface, err := net.InterfaceByIndex(ifindex)
		if err != nil {
			tun.errors <- err
			return
		}

		// Up / Down event
		up := (iface.Flags & net.FlagUp) != 0
		if up != statusUp && up {
			tun.events <- TUNEventUp
		}
		if up != statusUp && !up {
			tun.events <- TUNEventDown
		}
		statusUp = up

		// MTU changes
		if iface.MTU != statusMTU {
			tun.events <- TUNEventMTUUpdate
		}
		statusMTU = iface.MTU
	}
}

func tunName(fd uintptr) (string, error) {
	//Terrible hack to make up for freebsd not having a TUNGIFNAME

	//First, make sure the tun pid matches this proc's pid
	_, _, errno := unix.Syscall(
		unix.SYS_IOCTL,
		uintptr(fd),
		uintptr(_TUNSIFPID),
		uintptr(0),
	)

	if errno != 0 {
		return "", fmt.Errorf("failed to set tun device PID: %s", errno.Error())
	}

	// Open iface control socket

	confd, err := unix.Socket(
		unix.AF_INET,
		unix.SOCK_DGRAM,
		0,
	)

	if err != nil {
		return "", err
	}

	defer unix.Close(confd)

	procPid := os.Getpid()

	//Try to find interface with matching PID
	for i := 1; ; i++ {
		iface, _ := net.InterfaceByIndex(i)
		if err != nil || iface == nil {
			break
		}

		// Structs for getting data in and out of SIOCGIFSTATUS ioctl
		var ifstatus ifstat
		copy(ifstatus.IfsName[:], iface.Name)

		// Make the syscall to get the status string
		_, _, errno := unix.Syscall(
			unix.SYS_IOCTL,
			uintptr(confd),
			uintptr(unix.SIOCGIFSTATUS),
			uintptr(unsafe.Pointer(&ifstatus)),
		)

		if errno != 0 {
			continue
		}

		nullStr := ifstatus.Ascii[:]
		i := bytes.IndexByte(nullStr, 0)
		if i < 1 {
			continue
		}
		statStr := string(nullStr[:i])
		var pidNum int = 0

		// Finally get the owning PID
		// Format string taken from sys/net/if_tun.c
		_, err := fmt.Sscanf(statStr, "\tOpened by PID %d\n", &pidNum)
		if err != nil {
			continue
		}

		if pidNum == procPid {
			return iface.Name, nil
		}
	}

	return "", nil
}

// Rename an interface with SIOCSIFNAME
func renameTun(oldName string, newName string) (error) {
	// Open control socket
	confd, err := unix.Socket(
		unix.AF_INET,
		unix.SOCK_DGRAM,
		0,
	)

	if err != nil {
		return err
	}

	defer unix.Close(confd)
	
	// set up struct for iface rename
	var newnp [unix.IFNAMSIZ]byte
	copy(newnp[:], newName)

	var ifr ifreq_ptr
	copy(ifr.Name[:], oldName)
	ifr.Data = uintptr(unsafe.Pointer(&newnp[0]))

	//do actual ioctl to rename iface
	_, _, errno := unix.Syscall(
		unix.SYS_IOCTL,
		uintptr(confd),
		uintptr(unix.SIOCSIFNAME),
		uintptr(unsafe.Pointer(&ifr)),
	)
	if errno != 0 {
		return fmt.Errorf("failed to rename %s to %s: %s", oldName, newName, errno.Error())
	}
	
	return nil
}

func CreateTUN(name string, mtu int) (TUNDevice, error) {
	if len(name) > unix.IFNAMSIZ-1 {
		return nil, errors.New("interface name too long")
	}

	// See if interface already exists
	iface, err := net.InterfaceByName(name)
	if iface != nil {
		return nil, fmt.Errorf("interface %s already exists", name)
	}
	
	var tunfile *os.File
	
	// Try and open a usable tun device
	for i := 0; i < 99; i++ {
		// Try opening the tun dev file
		tunfile, err = os.OpenFile(fmt.Sprintf("/dev/tun%d",i), unix.O_RDWR, 0)
		
		// If we don't get an EBUSY or ENOEXIST, this should be a valid, usable tunnel interface
		if err == nil{
			break
		}
	}
	// If we've not found a free /dev/tunN interface, open a new one
	if tunfile == nil {
		tunfile, err = os.OpenFile("/dev/tun", unix.O_RDWR, 0)

		if err != nil {
			return nil, err
		}
	}
	
	tunfd := tunfile.Fd()
	
	assignedName, err := tunName(tunfd)
	if err != nil {
		tunfile.Close()
		return nil, err
	}

	// Enable ifhead mode, otherwise tun will complain if it gets a non-AF_INET packet
	ifheadmode := 1
	_, _, errno := unix.Syscall(
		unix.SYS_IOCTL,
		uintptr(tunfd),
		uintptr(_TUNSIFHEAD),
		uintptr(unsafe.Pointer(&ifheadmode)),
	)

	if errno != 0 {
		return nil, fmt.Errorf("error %s", errno.Error())
	}

	// Set TUN iface to broadcast mode. TUN inferfaces on dragonflybsd come up in point to point by default
	ifmodemode := unix.IFF_BROADCAST
	_, _, errno = unix.Syscall(
		unix.SYS_IOCTL,
		uintptr(tunfd),
		uintptr(_TUNSIFMODE),
		uintptr(unsafe.Pointer(&ifmodemode)),
	)

	if errno != 0 {
		return nil, fmt.Errorf("error %s", errno.Error())
	}
	
	// Rename tun interface
	err = renameTun(assignedName,name)
	
	if err != nil {
		tunfile.Close()
		return nil, err
	}
	
	var natTun *nativeTun;
	natTun, err = CreateTUNFromFile(tunfile, mtu)
	
	if err != nil {
		tunfile.Close()
		return nil, err
	}
	
	natTun.origName = assignedName
	
	return natTun, err
}

func CreateTUNFromFile(file *os.File, mtu int) (*nativeTun, error) {

	tun := &nativeTun{
		fd:     file,
		events: make(chan TUNEvent, 10),
		errors: make(chan error, 1),
	}

	name, err := tun.Name()
	if err != nil {
		tun.fd.Close()
		return nil, err
	}
	tun.origName = name
	
	tunIfindex, err := func() (int, error) {
		iface, err := net.InterfaceByName(name)
		if err != nil {
			return -1, err
		}
		return iface.Index, nil
	}()
	if err != nil {
		tun.fd.Close()
		return nil, err
	}

	tun.rwcancel, err = rwcancel.NewRWCancel(int(file.Fd()))
	if err != nil {
		tun.fd.Close()
		return nil, err
	}

	tun.routeSocket, err = unix.Socket(unix.AF_ROUTE, unix.SOCK_RAW, unix.AF_UNSPEC)
	if err != nil {
		tun.fd.Close()
		return nil, err
	}

	go tun.routineRouteListener(tunIfindex)

	err = tun.setMTU(mtu)
	if err != nil {
		tun.Close()
		return nil, err
	}

	return tun, nil
}

func (tun *nativeTun) Name() (string, error) {
	name, err := tunName(tun.fd.Fd())
	if err != nil {
		return "", err
	}
	tun.name = name
	return name, nil
}

func (tun *nativeTun) File() *os.File {
	return tun.fd
}

func (tun *nativeTun) Events() chan TUNEvent {
	return tun.events
}

func (tun *nativeTun) doRead(buff []byte, offset int) (int, error) {
	select {
	case err := <-tun.errors:
		return 0, err
	default:
		buff := buff[offset-4:]
		n, err := tun.fd.Read(buff[:])
		if n < 4 {
			return 0, err
		}
		return n - 4, err
	}
}

func (tun *nativeTun) Read(buff []byte, offset int) (int, error) {
	for {
		n, err := tun.doRead(buff, offset)
		if err == nil || !rwcancel.RetryAfterError(err) {
			return n, err
		}
		if !tun.rwcancel.ReadyRead() {
			return 0, errors.New("tun device closed")
		}
	}
}

func (tun *nativeTun) Write(buff []byte, offset int) (int, error) {

	// reserve space for header

	buff = buff[offset-4:]

	// add packet information header

	buff[0] = 0x00
	buff[1] = 0x00
	buff[2] = 0x00

	if buff[4]>>4 == ipv6.Version {
		buff[3] = unix.AF_INET6
	} else {
		buff[3] = unix.AF_INET
	}

	// write

	return tun.fd.Write(buff)
}

func (tun *nativeTun) Close() error {
	var err4 error
	// Rename tun back to original name if it's been changed
	if tun.name != tun.origName {
		err4 = renameTun(tun.name,tun.origName)
	}
	
	err2 := tun.fd.Close()
	err1 := tun.rwcancel.Cancel()
	if tun.routeSocket != -1 {
		unix.Shutdown(tun.routeSocket, unix.SHUT_RDWR)
		err4 = unix.Close(tun.routeSocket)
		tun.routeSocket = -1
	} else if tun.events != nil {
		close(tun.events)
	}
	if err1 != nil {
		return err1
	}
	if err2 != nil {
		return err2
	}
	return err4
}

func (tun *nativeTun) setMTU(n int) error {
	// open datagram socket

	var fd int

	fd, err := unix.Socket(
		unix.AF_INET,
		unix.SOCK_DGRAM,
		0,
	)

	if err != nil {
		return err
	}

	defer unix.Close(fd)

	// do ioctl call

	var ifr ifreq_mtu
	copy(ifr.Name[:], tun.name)
	ifr.MTU = uint32(n)

	_, _, errno := unix.Syscall(
		unix.SYS_IOCTL,
		uintptr(fd),
		uintptr(unix.SIOCSIFMTU),
		uintptr(unsafe.Pointer(&ifr)),
	)

	if errno != 0 {
		return fmt.Errorf("failed to set MTU on %s", tun.name)
	}

	return nil
}

func (tun *nativeTun) MTU() (int, error) {
	// open datagram socket

	fd, err := unix.Socket(
		unix.AF_INET,
		unix.SOCK_DGRAM,
		0,
	)

	if err != nil {
		return 0, err
	}

	defer unix.Close(fd)

	// do ioctl call
	var ifr ifreq_mtu
	copy(ifr.Name[:], tun.name)

	_, _, errno := unix.Syscall(
		unix.SYS_IOCTL,
		uintptr(fd),
		uintptr(unix.SIOCGIFMTU),
		uintptr(unsafe.Pointer(&ifr)),
	)
	if errno != 0 {
		return 0, fmt.Errorf("failed to get MTU on %s", tun.name)
	}

	return int(*(*int32)(unsafe.Pointer(&ifr.MTU))), nil
}
