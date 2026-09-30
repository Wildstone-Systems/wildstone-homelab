//go:build !linux

package storage

import "io"

type hooks struct{}

func (*Store) Stage(string, io.Reader) (Object, error) { return Object{}, ErrUnsupported }
func (*Store) Publish(Object, func() error) error      { return ErrUnsupported }
func (*Store) Read(Object) ([]byte, error)             { return nil, ErrUnsupported }
