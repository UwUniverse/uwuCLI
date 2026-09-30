/*
 * SPDX-FileCopyrightText: The uwuAOSP Project
 * SPDX-License-Identifier: Apache-2.0
 */

package uni

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
)

type compactStatusEvent struct {
	Type                  string `json:"type"`
	ID                    uint64 `json:"id,omitempty"`
	Description           string `json:"description,omitempty"`
	StartedUnixNano       int64  `json:"started_unix_nano,omitempty"`
	Total                 int    `json:"total,omitempty"`
	EstimatedTimeUnixNano int64  `json:"estimated_time_unix_nano,omitempty"`
}

type compactStatusServer struct {
	directory string
	socket    string
	listener  *net.UnixListener
	tui       *compactTUI
	done      chan struct{}
	workers   sync.WaitGroup
}

func startCompactStatusServer(tui *compactTUI) (*compactStatusServer, error) {
	directory, err := os.MkdirTemp("", "uni-status-")
	if err != nil {
		return nil, fmt.Errorf("create status directory: %w", err)
	}
	socket := filepath.Join(directory, "status.sock")
	address, err := net.ResolveUnixAddr("unix", socket)
	if err != nil {
		_ = os.RemoveAll(directory)
		return nil, fmt.Errorf("resolve status socket: %w", err)
	}
	listener, err := net.ListenUnix("unix", address)
	if err != nil {
		_ = os.RemoveAll(directory)
		return nil, fmt.Errorf("listen on status socket: %w", err)
	}
	server := &compactStatusServer{
		directory: directory,
		socket:    socket,
		listener:  listener,
		tui:       tui,
		done:      make(chan struct{}),
	}
	go server.accept()
	return server, nil
}

func (server *compactStatusServer) accept() {
	defer close(server.done)
	var connectionID uint64
	for {
		connection, err := server.listener.AcceptUnix()
		if err != nil {
			return
		}
		connectionID++
		server.workers.Add(1)
		go server.read(connection, connectionID)
	}
}

func (server *compactStatusServer) read(connection *net.UnixConn, connectionID uint64) {
	defer server.workers.Done()
	defer connection.Close()
	scanner := bufio.NewScanner(connection)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	for scanner.Scan() {
		var event compactStatusEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err == nil {
			server.tui.consumeStatusEvent(event, connectionID)
		}
	}
	server.tui.consumeStatusEvent(compactStatusEvent{Type: "disconnect"}, connectionID)
}

func (server *compactStatusServer) close() {
	if server == nil {
		return
	}
	_ = server.listener.Close()
	<-server.done
	server.workers.Wait()
	_ = os.RemoveAll(server.directory)
}
