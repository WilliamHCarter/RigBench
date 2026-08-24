package main

import "encoding/json"

func unmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }
