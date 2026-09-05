// sashiki op: operation の照会(仕様 17章)。
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"text/tabwriter"
	"time"
)

func cmdOp(args []string) int {
	if len(args) < 1 {
		return usageOp()
	}
	switch args[0] {
	case "list":
		return opList()
	case "show":
		if len(args) < 2 {
			return usageOp()
		}
		return opShow(args[1])
	case "wait":
		if len(args) < 2 {
			return usageOp()
		}
		return opWait(args[1])
	default:
		return usageOp()
	}
}

func usageOp() int {
	fmt.Fprint(os.Stderr, "Usage:\n  sashiki op list\n  sashiki op show <id>\n  sashiki op wait <id>\n")
	return exitUsage
}

type opView struct {
	ID         string  `json:"id"`
	Type       string  `json:"type"`
	Target     string  `json:"target"`
	State      string  `json:"state"`
	StartedAt  string  `json:"started_at"`
	FinishedAt *string `json:"finished_at"`
	Error      string  `json:"error"`
}

func opList() int {
	code, data, err := call("GET", "/v1/operations", nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sashiki:", err)
		return exitError
	}
	if code != http.StatusOK {
		fmt.Fprintln(os.Stderr, "sashiki:", apiError(data))
		return statusToExit(code)
	}
	var resp struct {
		Operations []opView `json:"operations"`
	}
	_ = json.Unmarshal(data, &resp)
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tTYPE\tTARGET\tSTATE\tSTARTED")
	for _, o := range resp.Operations {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", o.ID, o.Type, o.Target, o.State, o.StartedAt)
	}
	_ = tw.Flush()
	return exitOK
}

func opShow(id string) int {
	code, data, err := call("GET", "/v1/operations/"+id, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sashiki:", err)
		return exitError
	}
	if code != http.StatusOK {
		fmt.Fprintln(os.Stderr, "sashiki:", apiError(data))
		return statusToExit(code)
	}
	fmt.Println(string(data))
	return exitOK
}

func opWait(id string) int {
	for i := 0; i < 3000; i++ { // 最大 ~10 分
		code, data, err := call("GET", "/v1/operations/"+id, nil)
		if err != nil {
			fmt.Fprintln(os.Stderr, "sashiki:", err)
			return exitError
		}
		if code != http.StatusOK {
			fmt.Fprintln(os.Stderr, "sashiki:", apiError(data))
			return statusToExit(code)
		}
		var o opView
		_ = json.Unmarshal(data, &o)
		if o.State != "running" {
			fmt.Printf("%s: %s\n", o.ID, o.State)
			if o.State == "failed" {
				fmt.Fprintln(os.Stderr, o.Error)
				return exitError
			}
			return exitOK
		}
		time.Sleep(200 * time.Millisecond)
	}
	fmt.Fprintln(os.Stderr, "sashiki: wait timed out")
	return exitError
}
