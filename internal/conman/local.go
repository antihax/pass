package conman

import (
	"net/http"
	_ "net/http/pprof" // Force pprof to load
)

func runPProf() {
	http.ListenAndServe("localhost:9900", nil)
}
