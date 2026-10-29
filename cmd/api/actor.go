package main

import "net/http"

// requestActor is replaced by session authentication when browser sign-in is enabled.
func requestActor(r *http.Request) string { return "bearer" }
