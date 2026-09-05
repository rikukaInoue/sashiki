// sashiki capacity: memory / storage / ports の空き表示(仕様 14-2)。
package main

import (
	"fmt"
	"net/http"
	"os"
)

func cmdCapacity(args []string) int {
	code, data, err := call("GET", "/v1/capacity", nil)
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
