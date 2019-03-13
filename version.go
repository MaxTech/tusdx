package tusdx

import "fmt"

var Version string

func init() {
    Version = fmt.Sprintf(
        "|- %s service:\t\t\t%s",
        "tusdx",
        "0.0.1")
}
