// Command keystatus prints "embedded" when a release public key is compiled in
// (so shipped binaries REQUIRE a valid signature) and "none" otherwise. CI uses
// it to decide whether an unsigned release would break `calvoproxy update`.
package main

import (
	"fmt"
	"strings"

	"github.com/cervantesh/calvoproxy/internal/releasekey"
)

func main() {
	if strings.TrimSpace(releasekey.Public) != "" {
		fmt.Println("embedded")
		return
	}
	fmt.Println("none")
}
