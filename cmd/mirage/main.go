// Command mirage is a service virtualization and traffic replay platform for
// performance testing. The integration step replaces this placeholder with
// the real CLI (serve, replay, anonymize, import, export, version).
package main

import (
	"fmt"

	"github.com/justinburger/mirage/internal/version"
)

func main() {
	fmt.Println("mirage", version.String())
}
