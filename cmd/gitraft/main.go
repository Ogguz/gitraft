package main

import "fmt"

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	fmt.Printf("gitraft %s (%s, %s)\n", version, commit, date)
}
