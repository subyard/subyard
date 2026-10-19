//go:build ignore

// Native transport fault peer; no release semantics or effects.
package main

import (
	"encoding/binary"
	"encoding/json"
	"io"
	"os"
	"time"
)

func main() {
	for {
		h := make([]byte, 52)
		if _, e := io.ReadFull(os.Stdin, h); e != nil {
			return
		}
		n := binary.BigEndian.Uint32(h[48:])
		if n > 1<<20 {
			return
		}
		raw := make([]byte, n)
		if _, e := io.ReadFull(os.Stdin, raw); e != nil {
			return
		}
		if len(raw) == 0 {
			return
		}
		switch raw[0] {
		case 't':
			time.Sleep(20 * time.Second)
			return
		case 'e':
			return
		case 'x':
			os.Exit(7)
		case 'n':
			h[15] ^= 1
		case 'k':
			h[5] ^= 1
		case 'h':
			h[16] ^= 1
		case 's':
			h[6] = 2
		case 'f':
			h[7] = 1
		case 'b':
			binary.BigEndian.PutUint32(h[48:], (8<<20)+1)
			os.Stdout.Write(h)
			return
		case 'd':
			os.Stdout.Write(h[:12])
			return
		}
        if h[5] == 15 {
            var request struct { Op string `json:"op"`; RecipeHash string `json:"recipeHash"`; Offset uint64 `json:"offset"`; Total uint64 `json:"total"` }
            if json.Unmarshal(raw,&request)!=nil {return}
            chunk:=map[string]any{"recipeHash":request.RecipeHash,"offset":request.Offset,"total":request.Offset+5,"content":"aGVsbG8=","complete":true}
            reply:=map[string]any{"code":"","installedChunk":chunk}
            switch request.Offset {case 1:chunk["offset"]=2;case 2:chunk["total"]=0;case 3:chunk["content"]="%%%";case 4:chunk["complete"]=false;case 5:reply["code"]="stale";case 6:chunk["total"]=uint64(49<<20);case 7:reply=map[string]any{"code":"stale"}}
            if request.Op=="bind-installed-recipe" {chunk["content"]="";chunk["total"]=request.Total;chunk["complete"]=request.Offset!=8}
            projection,_:=json.Marshal(reply);binary.BigEndian.PutUint32(h[48:],uint32(len(projection)));os.Stdout.Write(h);os.Stdout.Write(projection);continue
        }
		projection := []byte(`{}`)
		switch raw[0] {
		case 'm':
			projection = []byte(`{`)
		case 'w':
			projection = []byte(`{"schemaVersion":"wrong-type"}`)
		case 'a':
			projection = []byte(`[]`)
		case 'r', 'R':
			h[6] = 1
			projection = nil
			if raw[0] == 'r' {
				h[7] = 1
			}
		case 'p':
			h[7] = 1
			projection = []byte(`{"schemaVersion":1}`)
		}
		binary.BigEndian.PutUint32(h[48:], uint32(len(projection)))
		os.Stdout.Write(h)
		os.Stdout.Write(projection)
	}
}
