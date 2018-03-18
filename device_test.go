package main

/* Create two device instances and simulate full WireGuard interaction
 * without network dependencies
 */

import "testing"

func TestDevice(t *testing.T) {

	// prepare tun devices for generating traffic

	tun1, err := CreateDummyTUN("tun1")
	if err != nil {
		t.Error("failed to create tun:", err)
	}

	tun2, err := CreateDummyTUN("tun2")
	if err != nil {
		t.Error("failed to create tun:", err)
	}

	println(tun1)
	println(tun2)

	// prepare networking

	network, err := CreateDummyNetworking()
	if err != nil {
		t.Error("failed to prepare networking:", err)
	}

	println(network)

	// prepare endpoints

	end1, err := CreateDummyEndpoint()
	if err != nil {
		t.Error("failed to create endpoint:", err.Error())
	}

	end2, err := CreateDummyEndpoint()
	if err != nil {
		t.Error("failed to create endpoint:", err.Error())
	}

	println(end1)
	println(end2)

	// create binds

}
