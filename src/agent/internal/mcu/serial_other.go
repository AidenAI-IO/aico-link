//go:build !linux

package mcu

import (
	"errors"
	"io"
)

func OpenSerial(string) (io.ReadWriteCloser, error) {
	return nil, errors.New("MCU UART requires Linux")
}
func DrainSerial(io.ReadWriteCloser) error { return errors.New("MCU UART requires Linux") }
