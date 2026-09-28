package main

import (
	"reflect"
	"testing"
)

func TestSplitPassthrough(t *testing.T) {
	cases := []struct {
		args       []string
		head, tail []string
	}{
		{[]string{"m"}, []string{"m"}, nil},
		{[]string{"m", "--flash-attn"}, []string{"m", "--flash-attn"}, nil},
		{[]string{"m", "--", "--flash-attn", "--cache-type-k", "q8_0"},
			[]string{"m"}, []string{"--flash-attn", "--cache-type-k", "q8_0"}},
		{[]string{"--events", "m", "--", "-ngl", "999"},
			[]string{"--events", "m"}, []string{"-ngl", "999"}},
		{[]string{"m", "--"}, []string{"m"}, []string{}},
	}
	for _, c := range cases {
		head, tail := splitPassthrough(c.args)
		if !reflect.DeepEqual(head, c.head) {
			t.Errorf("splitPassthrough(%v) head=%v, want %v", c.args, head, c.head)
		}
		if !reflect.DeepEqual(tail, c.tail) {
			t.Errorf("splitPassthrough(%v) tail=%v, want %v", c.args, tail, c.tail)
		}
	}
}
