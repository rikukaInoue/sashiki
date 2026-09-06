package main

import (
	"net/http"
	"testing"
)

func TestStatusToExit(t *testing.T) {
	cases := map[int]int{
		http.StatusNotFound:            exitNotFound,
		http.StatusConflict:            exitExists,
		http.StatusInsufficientStorage: exitCapacity, // 仕様 18章: capacity 不足は 5
		http.StatusInternalServerError: exitError,
	}
	for in, want := range cases {
		if got := statusToExit(in); got != want {
			t.Errorf("statusToExit(%d) = %d, want %d", in, got, want)
		}
	}
}
