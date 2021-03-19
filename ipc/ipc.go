// mautrix-imessage - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2021 Tulir Asokan
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"sync"
	"sync/atomic"

	log "maunium.net/go/maulogger/v2"
)

const (
	CommandResponse = "response"
	CommandError    = "error"
)

var (
	ErrUnknownCommand = errors.New("unknown command")
)

type Command string

type Message struct {
	Command Command         `json:"command"`
	ID      int             `json:"id"`
	Data    json.RawMessage `json:"data"`
}

type OutgoingMessage struct {
	Command Command     `json:"command"`
	ID      int         `json:"id,omitempty"`
	Data    interface{} `json:"data,omitempty"`
}

type HandlerFunc func(message json.RawMessage) interface{}

type Processor struct {
	log    log.Logger
	lock   *sync.Mutex
	stdout *json.Encoder
	stdin  *json.Decoder

	handlers   map[Command]HandlerFunc
	waiters    map[int]chan<- *Message
	waiterLock sync.Mutex
	reqID      int32
}

func NewProcessor(logger log.Logger) *Processor {
	return &Processor{
		lock:     &logger.(*log.BasicLogger).StdoutLock,
		log:      logger.Sub("IPC"),
		stdout:   json.NewEncoder(os.Stdout),
		stdin:    json.NewDecoder(os.Stdin),
		handlers: make(map[Command]HandlerFunc),
		waiters:  make(map[int]chan<- *Message),
	}
}

func (ipc *Processor) Loop() {
	for {
		var msg Message
		err := ipc.stdin.Decode(&msg)
		if err == io.EOF {
			ipc.log.Debugln("Standard input closed, ending IPC loop")
			break
		} else if err != nil {
			ipc.log.Errorln("Failed to read input:", err)
			break
		}

		ipc.log.Debugfln("Received IPC command: %+v", msg)
		if msg.Command == "response" || msg.Command == "error" {
			ipc.waiterLock.Lock()
			waiter, ok := ipc.waiters[msg.ID]
			if !ok {
				ipc.log.Warnln("Nothing waiting for IPC response to %d", msg.ID)
			} else {
				delete(ipc.waiters, msg.ID)
				waiter <- &msg
			}
			ipc.waiterLock.Unlock()
		} else {
			handler, ok := ipc.handlers[msg.Command]
			if !ok {
				ipc.respond(msg.ID, ErrUnknownCommand)
			} else {
				go ipc.callHandler(&msg, handler)
			}
		}
	}
}

func (ipc *Processor) Request(cmd Command, data interface{}) (<-chan *Message, error) {
	respChan := make(chan *Message, 1)
	reqID := int(atomic.AddInt32(&ipc.reqID, 1))
	ipc.waiterLock.Lock()
	ipc.waiters[reqID] = respChan
	ipc.waiterLock.Unlock()
	ipc.lock.Lock()
	err := ipc.stdout.Encode(OutgoingMessage{Command: cmd, ID: reqID, Data: data})
	ipc.lock.Unlock()
	if err != nil {
		ipc.waiterLock.Lock()
		delete(ipc.waiters, reqID)
		ipc.waiterLock.Unlock()
		close(respChan)
	}
	return respChan, err
}

type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (err Error) Error() string {
	return fmt.Sprintf("%s: %s", err.Code, err.Message)
}

func (ipc *Processor) RequestWait(ctx context.Context, cmd Command, reqData interface{}, respData interface{}) error {
	respChan, err := ipc.Request(cmd, reqData)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	select {
	case rawData := <-respChan:
		if rawData.Command == "error" {
			var respErr Error
			err = json.Unmarshal(rawData.Data, &respErr)
			if err != nil {
				return fmt.Errorf("failed to parse error response: %w", err)
			}
			return respErr
		}
		if respData != nil {
			err = json.Unmarshal(rawData.Data, &respData)
			if err != nil {
				return fmt.Errorf("failed to parse response: %w", err)
			}
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("context finished: %w", ctx.Err())
	}
}

func (ipc *Processor) callHandler(msg *Message, handler HandlerFunc) {
	defer func() {
		err := recover()
		if err != nil {
			ipc.log.Errorfln("Panic in IPC handler for %s: %v:\n%s", msg.Command, err, string(debug.Stack()))
			ipc.respond(msg.ID, err)
		}
	}()
	resp := handler(msg.Data)
	ipc.respond(msg.ID, resp)
}

func (ipc *Processor) respond(id int, response interface{}) {
	if id == 0 && response == nil {
		// No point in replying
		return
	}
	resp := OutgoingMessage{Command: CommandResponse, ID: id, Data: response}
	respErr, isError := response.(error)
	if isError {
		resp.Data = respErr.Error()
		resp.Command = CommandError
	}
	ipc.lock.Lock()
	err := ipc.stdout.Encode(resp)
	ipc.lock.Unlock()
	if err != nil {
		ipc.log.Errorln("Failed to encode IPC response: %v", err)
	}
}

func (ipc *Processor) SetHandler(command Command, handler HandlerFunc) {
	ipc.handlers[command] = handler
}
