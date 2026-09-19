package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type request struct {
	ID     string         `json:"id"`
	Method string         `json:"method"`
	Params map[string]any `json:"params"`
}

type response struct {
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) < 3 {
		return errors.New("usage: voice-herdr-report <state|session> <pane-id> <value> [detail]")
	}
	pane := args[1]
	if pane == "" || strings.ContainsAny(pane, " \t\r\n/") {
		return errors.New("invalid Herdr pane id")
	}
	now := time.Now().UnixNano()
	params := map[string]any{
		"pane_id": pane,
		"source":  "herdr:omp",
		"agent":   "omp",
		"seq":     now,
	}
	req := request{ID: fmt.Sprintf("voice:omp:%d", now), Params: params}

	switch args[0] {

	case "state":
		if len(args) < 4 || len(args) > 5 || !filepath.IsAbs(args[3]) {
			return errors.New("state report requires an absolute OMP session path and optional message")
		}
		switch args[2] {
		case "working", "blocked", "idle":
			params["state"] = args[2]
		default:
			return fmt.Errorf("invalid Herdr agent state %q", args[2])
		}
		params["agent_session_path"] = args[3]
		if len(args) == 5 && args[4] != "" {
			params["message"] = args[4]
		}
		req.Method = "pane.report_agent"
	case "session":
		if len(args) > 4 || !filepath.IsAbs(args[2]) {
			return errors.New("session report requires an absolute OMP session path and optional source")
		}
		params["agent_session_path"] = args[2]
		params["session_start_source"] = "startup"
		if len(args) == 4 && args[3] != "" {
			params["session_start_source"] = args[3]
		}
		req.Method = "pane.report_agent_session"
	default:
		return fmt.Errorf("unsupported report type %q", args[0])
	}
	return send(req)
}

func send(req request) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve Herdr owner home: %w", err)
	}
	conn, err := net.DialTimeout("unix", filepath.Join(home, ".config", "herdr", "herdr.sock"), 2*time.Second)
	if err != nil {
		return fmt.Errorf("connect to Herdr: %w", err)
	}
	defer conn.Close() // discard: closing a local socket cannot recover the completed report
	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return fmt.Errorf("set Herdr deadline: %w", err)
	}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return fmt.Errorf("send Herdr report: %w", err)
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return fmt.Errorf("read Herdr response: %w", err)
	}
	var res response
	if err := json.Unmarshal(line, &res); err != nil {
		return fmt.Errorf("decode Herdr response: %w", err)
	}
	if res.Error != nil {
		return fmt.Errorf("Herdr rejected report (%s): %s", res.Error.Code, res.Error.Message)
	}
	return nil
}
