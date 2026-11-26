package main

import (
	"fmt"
	"os"

	"github.com/PedroCarassale/nimbus/compute-plane/internal/fcguest"
)

func main() {
	if err := fcguest.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "nimbus-init: %v\n", err)
		os.Exit(1)
	}
}
