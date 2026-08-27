package main

import "testing"

func TestNonNegativeIntegerAcceptsZero(t *testing.T) {
	t.Setenv("TEST_NON_NEGATIVE_INTEGER", "0")
	if got := nonNegativeInteger("TEST_NON_NEGATIVE_INTEGER", 10); got != 0 {
		t.Fatalf("got %d", got)
	}
}
