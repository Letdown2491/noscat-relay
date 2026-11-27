package main

import "fmt"

var (
	major = 0
	minor = 3
	patch = 0
	meta  = ""
)

func StringVersion() string {
	v := fmt.Sprintf("noscat-relay %d.%d.%d", major, minor, patch)
	if meta != "" {
		v = fmt.Sprintf("%s-%s", v, meta)
	}
	return v
}
