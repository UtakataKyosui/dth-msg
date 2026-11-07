//go:build js && wasm

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"syscall/js"

	"github.com/UtakataKyosui/dth-msg/internal/monitor"
)

type workerStore struct {
	state monitor.State
	save  js.Value
}

func (s *workerStore) Load(context.Context) (monitor.State, error) {
	return s.state, nil
}

func (s *workerStore) Save(_ context.Context, state monitor.State) error {
	body, err := json.Marshal(state)
	if err != nil {
		return err
	}
	done := make(chan error, 1)
	ok := js.FuncOf(func(js.Value, []js.Value) any { done <- nil; return nil })
	fail := js.FuncOf(func(js.Value, []js.Value) any {
		done <- errors.New("durable storage failed")
		return nil
	})
	defer ok.Release()
	defer fail.Release()
	s.save.Invoke(string(body)).Call("then", ok, fail)
	if err := <-done; err != nil {
		return err
	}
	s.state = state
	return nil
}

func main() {
	run := js.FuncOf(func(_ js.Value, args []js.Value) any {
		executor := js.FuncOf(func(_ js.Value, callbacks []js.Value) any {
			resolve := callbacks[0]
			go func() {
				var config monitor.Config
				store := &workerStore{save: args[6]}
				err := json.Unmarshal([]byte(args[0].String()), &config)
				if err == nil {
					err = json.Unmarshal([]byte(args[1].String()), &store.state)
				}
				var service *monitor.Service
				if err == nil {
					service, err = monitor.NewService(config, store)
				}
				if err != nil {
					resolve.Invoke(map[string]any{"status": 500, "body": `{"error":"invalid server configuration or state"}`})
					return
				}
				req := httptest.NewRequest(args[2].String(), "https://internal"+args[3].String(), strings.NewReader(args[4].String()))
				req.Header.Set("Authorization", args[5].String())
				res := httptest.NewRecorder()
				service.ServeHTTP(res, req)
				headers := map[string]any{}
				for key := range res.Header() {
					headers[key] = res.Header().Get(key)
				}
				resolve.Invoke(map[string]any{"status": res.Code, "body": res.Body.String(), "headers": headers})
			}()
			return nil
		})
		promise := js.Global().Get("Promise").New(executor)
		executor.Release()
		return promise
	})
	js.Global().Set("dthRequest", run)
	select {}
}
