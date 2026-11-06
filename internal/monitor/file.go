//go:build !js

package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
)

type FileStore struct {
	Path string
}

func (f FileStore) Load(context.Context) (State, error) {
	var s State
	body, err := os.ReadFile(f.Path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	err = json.Unmarshal(body, &s)
	return s, err
}

func (f FileStore) Save(_ context.Context, s State) error {
	body, err := json.Marshal(s)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(f.Path+".tmp", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	if err = file.Chmod(0600); err == nil {
		_, err = file.Write(body)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Path+".tmp", f.Path)
}
