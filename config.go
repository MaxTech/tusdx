package tusdx

import "github.com/tus/tusd"

var (
    Store    EitStore
    Composer *tusd.StoreComposer
)

func init() {
    Composer = tusd.NewStoreComposer()
}
