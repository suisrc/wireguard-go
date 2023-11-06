//go:build !linux

package device

import (
	"github.com/suisrc/wireguard-go/conn"
	"github.com/suisrc/wireguard-go/rwcancel"
)

func (device *Device) startRouteListener(bind conn.Bind) (*rwcancel.RWCancel, error) {
	return nil, nil
}
