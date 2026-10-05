//go:build !linux

package storage

import "io"

type hooks struct{}

func (*Store) Stage(string, io.Reader) (Object, error) { return Object{}, ErrUnsupported }
func (*Store) Publish(Object, func() error) error      { return ErrUnsupported }
func (*Store) Read(Object) ([]byte, error)             { return nil, ErrUnsupported }

func (*Store) StageObject(string, Object, io.Reader, func() error) error { return ErrUnsupported }
func (*Store) SyncStage(Object, func() error) ([]byte, error)            { return nil, ErrUnsupported }
func (*Store) SyncPublished(Object, func() error) ([]byte, error)        { return nil, ErrUnsupported }
