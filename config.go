package tusdx

import "github.com/tus/tusd"

var (
    Store    MaxStore
    Composer *tusd.StoreComposer
)

func init() {
    Composer = tusd.NewStoreComposer()
}
