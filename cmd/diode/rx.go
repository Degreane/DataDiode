package main

import (
	"context"
	"errors"
)

// runRx is implemented in S01-7. This stub keeps the binary buildable
// in the meantime so `diode --mode=tx` is usable end-to-end with a
// stand-in receiver (e.g. nc -u -l) on the other side.
func runRx(_ context.Context, _ []string) error {
	return errors.New("not implemented yet (S01-7)")
}
