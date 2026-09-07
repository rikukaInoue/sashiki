// sashiki: CLI。sashikid の REST API を叩く(仕様 14-5 の v0.1 サブセット)。
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"text/tabwriter"
	"time"
)

const (
	exitOK       = 0
	exitError    = 1
	exitUsage    = 2
	exitNotFound = 3
	exitExists   = 4
	exitCapacity = 5 // 507: capacity 不足(仕様 18章)
	exitTimeout  = 5 // op wait のタイムアウト(operation 失敗とは区別する, #83)
)

var version = "dev" // -ldflags で埋め込む

func main() {
	os.Exit(run(os.Args[1:]))
}

func usage() int {
	fmt.Fprint(os.Stderr, `Usage:
  sashiki create <name> [--port N] [--owner O] [--purpose P] [--source JSON] [--json]
  sashiki delete <name>
  sashiki reset  <name> [--json]
  sashiki recreate <name> [--json]
  sashiki retry <name> [--json]
  sashiki sleep  <name> [--json]
  sashiki wake   <name> [--json]
  sashiki hooks run <name> <event>
  sashiki list   [--json]
  sashiki show   <name> [--json]
  sashiki connect <name>
  sashiki init   --pool <p> [--device <dev>] [--skip-packages] [--yes]
  sashiki baseline import|list
  sashiki token create|list|revoke
  sashiki op list | show <id> | wait <id>
  sashiki capacity [--json]
  sashiki doctor [--json]
  sashiki gc --orphans
  sashiki drain
  sashiki version
`)
	return exitUsage
}

func run(args []string) int {
	if len(args) < 1 {
		return usage()
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "create":
		return cmdCreate(rest)
	case "delete":
		return cmdDelete(rest)
	case "reset":
		return cmdSimpleBranch(rest, "reset")
	case "recreate":
		return cmdSimpleBranch(rest, "recreate")
	case "retry":
		return cmdSimpleBranch(rest, "retry")
	case "sleep":
		return cmdSyncBranch(rest, "sleep")
	case "wake":
		return cmdSyncBranch(rest, "wake")
	case "lease":
		return cmdLease(rest)
	case "hooks":
		return cmdHooks(rest)
	case "list":
		return cmdList(rest)
	case "show":
		return cmdShow(rest)
	case "connect":
		return cmdConnect(rest)
	case "init":
		return cmdInit(rest)
	case "baseline":
		return cmdBaseline(rest)
	case "token":
		return cmdToken(rest)
	case "op":
		return cmdOp(rest)
	case "capacity":
		return cmdCapacity(rest)
	case "doctor":
		return cmdDoctor(rest)
	case "gc":
		return cmdGC(rest)
	case "drain":
		return cmdDrain(rest)
	case "version":
		fmt.Println("sashiki", version)
		return exitOK
	default:
		return usage()
	}
}

// --- API client ---

func apiURL() string {
	if v := os.Getenv("SASHIKI_API_URL"); v != "" {
		return v
	}
	return "http://127.0.0.1:8080"
}

func apiToken() string {
	if v := os.Getenv("SASHIKI_API_TOKEN"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(home + "/.config/sashiki/token")
	if err != nil {
		return ""
	}
	return string(bytes.TrimSpace(b))
}

func call(method, path string, body any) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, apiURL()+path, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if t := apiToken(); t != "" {
		req.Header.Set("Authorization", "Bearer "+t)
	}
	client := &http.Client{Timeout: 15 * time.Minute} // create/reset はストレージ次第で長い
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	return resp.StatusCode, data, err
}

func apiError(data []byte) string {
	var e struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &e) == nil && e.Error.Message != "" {
		return e.Error.Message
	}
	return string(data)
}

func statusToExit(code int) int {
	switch code {
	case http.StatusNotFound:
		return exitNotFound
	case http.StatusConflict:
		return exitExists
	case http.StatusInsufficientStorage:
		return exitCapacity
	default:
		return exitError
	}
}

type branchView struct {
	Name         string   `json:"name"`
	State        string   `json:"state"`
	Port         int      `json:"port"`
	Host         string   `json:"host"`
	User         string   `json:"user"`
	Profile      string   `json:"profile"`
	CreatedAt    string   `json:"created_at"`
	LastConnAt   *string  `json:"last_conn_at"`
	ExpiresAt    *string  `json:"expires_at"`
	UsedBytes    int64    `json:"used_bytes"`
	LogicalBytes int64    `json:"logical_bytes"`
	Error        string   `json:"error"`
	FailedOp     string   `json:"failed_operation"`
	ErrorCode    string   `json:"error_code"`
	Recoverable  bool     `json:"recoverable"`
	Suggestions  []string `json:"suggested_actions"`
}

// --- commands ---

func parseFlags(args []string) (pos []string, port int, jsonOut bool, err error) {
	pos, port, jsonOut, _, err = parseFlagsKV(args)
	return
}

// parseFlagsKV は --json / --port に加え、--owner/--purpose/--source/--profile を拾う。
func parseFlagsKV(args []string) (pos []string, port int, jsonOut bool, kv map[string]string, err error) {
	kv = map[string]string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "--json":
			jsonOut = true
		case "--port":
			if i+1 >= len(args) {
				return nil, 0, false, nil, fmt.Errorf("--port requires a value")
			}
			i++
			port, err = strconv.Atoi(args[i])
			if err != nil {
				return nil, 0, false, nil, fmt.Errorf("--port: %w", err)
			}
		case "--owner", "--purpose", "--source", "--profile", "--ttl", "--baseline":
			if i+1 >= len(args) {
				return nil, 0, false, nil, fmt.Errorf("%s requires a value", a)
			}
			i++
			kv[a[2:]] = args[i]
		default:
			pos = append(pos, a)
		}
	}
	return pos, port, jsonOut, kv, nil
}

func cmdCreate(args []string) int {
	args, noWait, timeout, interval := extractWaitFlags(args)
	pos, port, jsonOut, kv, err := parseFlagsKV(args)
	if err != nil || len(pos) != 1 {
		return usage()
	}
	body := map[string]any{"name": pos[0], "port": port}
	for _, k := range []string{"owner", "purpose", "profile", "ttl", "baseline"} {
		if v := kv[k]; v != "" {
			body[k] = v
		}
	}
	if src := kv["source"]; src != "" {
		body["source"] = json.RawMessage(src)
	}
	code, data, err := call("POST", "/v1/branches", body)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sashiki:", err)
		return exitError
	}
	switch code {
	case http.StatusOK: // exist_ok で既存 → branch がそのまま返る
		return printCreatedBranch(data, jsonOut)
	case http.StatusAccepted: // 非同期。既定で完了を待ってから branch を取得して表示
		done, exit := awaitMutation(data, noWait, timeout, interval)
		if !done {
			return exit
		}
		bcode, bdata, berr := call("GET", "/v1/branches/"+pos[0], nil)
		if berr != nil || bcode != http.StatusOK {
			fmt.Printf("branch '%s' ready\n", pos[0])
			return exitOK
		}
		return printCreatedBranch(bdata, jsonOut)
	default:
		fmt.Fprintln(os.Stderr, "sashiki:", apiError(data))
		return statusToExit(code)
	}
}

func printCreatedBranch(data []byte, jsonOut bool) int {
	if jsonOut {
		fmt.Println(string(data))
		return exitOK
	}
	var b branchView
	_ = json.Unmarshal(data, &b)
	fmt.Printf("branch '%s' ready: mysql -u%s -h %s -P%d\n", b.Name, b.User, b.Host, b.Port)
	return exitOK
}

func cmdDelete(args []string) int {
	args, noWait, timeout, interval := extractWaitFlags(args)
	if len(args) != 1 {
		return usage()
	}
	name := args[0]
	code, data, err := call("DELETE", "/v1/branches/"+name, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sashiki:", err)
		return exitError
	}
	if code != http.StatusAccepted {
		fmt.Fprintln(os.Stderr, "sashiki:", apiError(data))
		return statusToExit(code)
	}
	done, exit := awaitMutation(data, noWait, timeout, interval)
	if !done {
		return exit
	}
	fmt.Printf("branch '%s' deleted\n", name)
	return exitOK
}

// cmdLease は `sashiki lease renew <name> --for 7d`。expires_at を now+for に設定する。
func cmdLease(args []string) int {
	if len(args) < 1 || args[0] != "renew" {
		fmt.Fprintln(os.Stderr, "usage: sashiki lease renew <name> --for <dur>  (例 7d, 1h)")
		return usage()
	}
	rest := args[1:]
	var name, dur string
	var jsonOut bool
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case "--for":
			if i+1 >= len(rest) {
				fmt.Fprintln(os.Stderr, "sashiki: --for requires a value")
				return exitError
			}
			i++
			dur = rest[i]
		case "--json":
			jsonOut = true
		default:
			name = rest[i]
		}
	}
	if name == "" || dur == "" {
		fmt.Fprintln(os.Stderr, "usage: sashiki lease renew <name> --for <dur>  (例 7d, 1h)")
		return usage()
	}
	code, data, err := call("POST", "/v1/branches/"+name+"/lease", map[string]any{"for": dur})
	if err != nil {
		fmt.Fprintln(os.Stderr, "sashiki:", err)
		return exitError
	}
	if code != http.StatusOK {
		fmt.Fprintln(os.Stderr, "sashiki:", apiError(data))
		return statusToExit(code)
	}
	if jsonOut {
		fmt.Println(string(data))
		return exitOK
	}
	var b branchView
	_ = json.Unmarshal(data, &b)
	exp := "(none)"
	if b.ExpiresAt != nil {
		exp = *b.ExpiresAt
	}
	fmt.Printf("branch '%s' lease renewed: expires_at=%s\n", name, exp)
	return exitOK
}

// cmdSyncBranch は同期の変更操作(sleep / wake)を実行し branch を表示する(#87)。
func cmdSyncBranch(args []string, action string) int {
	pos, _, jsonOut, err := parseFlags(args)
	if err != nil || len(pos) != 1 {
		return usage()
	}
	code, data, err := call("POST", "/v1/branches/"+pos[0]+"/"+action, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sashiki:", err)
		return exitError
	}
	if code != http.StatusOK {
		fmt.Fprintln(os.Stderr, "sashiki:", apiError(data))
		return statusToExit(code)
	}
	if jsonOut {
		fmt.Println(string(data))
	} else {
		fmt.Printf("branch '%s' %s\n", pos[0], action)
	}
	return exitOK
}

func cmdSimpleBranch(args []string, action string) int {
	args, noWait, timeout, interval := extractWaitFlags(args)
	pos, _, jsonOut, err := parseFlags(args)
	if err != nil || len(pos) != 1 {
		return usage()
	}
	code, data, err := call("POST", "/v1/branches/"+pos[0]+"/"+action, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sashiki:", err)
		return exitError
	}
	if code != http.StatusAccepted {
		fmt.Fprintln(os.Stderr, "sashiki:", apiError(data))
		return statusToExit(code)
	}
	done, exit := awaitMutation(data, noWait, timeout, interval)
	if !done {
		return exit
	}
	if jsonOut {
		if _, bdata, e := call("GET", "/v1/branches/"+pos[0], nil); e == nil {
			fmt.Println(string(bdata))
		}
	} else {
		fmt.Printf("branch '%s' %s\n", pos[0], action)
	}
	return exitOK
}

func cmdList(args []string) int {
	_, _, jsonOut, err := parseFlags(args)
	if err != nil {
		return usage()
	}
	code, data, err := call("GET", "/v1/branches", nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sashiki:", err)
		return exitError
	}
	if code != http.StatusOK {
		fmt.Fprintln(os.Stderr, "sashiki:", apiError(data))
		return statusToExit(code)
	}
	if jsonOut {
		fmt.Println(string(data))
		return exitOK
	}
	var resp struct {
		Branches []branchView `json:"branches"`
	}
	_ = json.Unmarshal(data, &resp)
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "NAME\tPORT\tSTATE\tLAST_CONN\tUSED")
	for _, b := range resp.Branches {
		last := "-"
		if b.LastConnAt != nil {
			last = *b.LastConnAt
		}
		_, _ = fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\n", b.Name, b.Port, b.State, last, humanBytes(b.UsedBytes))
	}
	_ = tw.Flush()
	return exitOK
}

func cmdShow(args []string) int {
	pos, _, jsonOut, err := parseFlags(args)
	if err != nil || len(pos) != 1 {
		return usage()
	}
	code, data, err := call("GET", "/v1/branches/"+pos[0], nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sashiki:", err)
		return exitError
	}
	if code != http.StatusOK {
		fmt.Fprintln(os.Stderr, "sashiki:", apiError(data))
		return statusToExit(code)
	}
	if jsonOut {
		fmt.Println(string(data))
		return exitOK
	}
	var b branchView
	_ = json.Unmarshal(data, &b)
	fmt.Printf("name:    %s\nstate:   %s\nport:    %d\nuser:    %s\nprivate: %s (CoW差分)\nlogical: %s\n",
		b.Name, b.State, b.Port, b.User, humanBytes(b.UsedBytes), humanBytes(b.LogicalBytes))
	if b.Error != "" {
		fmt.Printf("error: %s\n", b.Error)
		if b.ErrorCode != "" {
			fmt.Printf("code:  %s (recoverable=%v)\n", b.ErrorCode, b.Recoverable)
		}
		for _, sug := range b.Suggestions {
			fmt.Printf("  → %s\n", sug)
		}
	}
	return exitOK
}

func cmdConnect(args []string) int {
	if len(args) != 1 {
		return usage()
	}
	code, data, err := call("GET", "/v1/branches/"+args[0], nil)
	if err != nil || code != http.StatusOK {
		fmt.Fprintln(os.Stderr, "sashiki:", apiError(data))
		return statusToExit(code)
	}
	var b branchView
	_ = json.Unmarshal(data, &b)
	mysqlPath, err := exec.LookPath("mysql")
	if err != nil {
		fmt.Fprintln(os.Stderr, "sashiki: mysql client not found in PATH")
		return exitError
	}
	argv := []string{"mysql", "-udev", "-pdev", "-h127.0.0.1", "-P" + strconv.Itoa(b.Port)}
	// CLI はそのまま mysql に化ける。
	if err := syscall.Exec(mysqlPath, argv, os.Environ()); err != nil {
		fmt.Fprintln(os.Stderr, "sashiki: exec mysql:", err)
		return exitError
	}
	return exitOK
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < 0 {
		return "-" // 不明(取得できない値。CoW 差分が取れない apfs 等)
	}
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%c", float64(n)/float64(div), "KMGTPE"[exp])
}
