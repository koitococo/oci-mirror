//go:build !linux

package main

import "errors"

func watchAuthFile(string) (<-chan struct{}, func(), error) {
	return nil, nil, errors.New("-upstream-authfile-watch requires Linux inotify")
}
