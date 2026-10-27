package cloudlogin

import (
	"net"
	"net/url"
)

func netListen(addr string) (net.Listener, error) {
	return net.Listen("tcp", addr)
}

type netTCPAddr = net.TCPAddr

var _ = url.Values{}
