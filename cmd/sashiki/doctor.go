// sashiki doctor / gc --orphans(仕様 20-2)。
package main

import (
	"fmt"
	"net/http"
	"os"
)

func cmdDoctor(args []string) int {
	code, data, err := call("GET", "/v1/doctor", nil)
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

func cmdGC(args []string) int {
	orphans := false
	for _, a := range args {
		if a == "--orphans" {
			orphans = true
		}
	}
	if !orphans {
		fmt.Fprintln(os.Stderr, "Usage: sashiki gc --orphans")
		return exitUsage
	}
	code, data, err := call("POST", "/v1/gc/orphans", nil)
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
