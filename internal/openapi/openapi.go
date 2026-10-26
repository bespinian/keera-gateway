// Package openapi serves the OpenAPI description of every route the gateway
// answers: the inference, control and sandbox APIs, and the probes.
//
// The spec is written by hand in openapi.yaml. A test in internal/server fails
// when a route is missing from it, or when it describes one that is gone.
package openapi

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/bespinian/keera-gateway/internal/version"
)

//go:embed openapi.yaml
var source []byte

// The paths it is served at, on the root like /healthz, because it describes
// more than the control API.
const (
	YAMLPath = "/openapi.yaml"
	JSONPath = "/openapi.json"
)

// Spec is the parsed spec, with info.version set to this build's version, so a
// client generated from it can say which gateway it was made for.
func Spec() (*yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(source, &doc); err != nil {
		return nil, err
	}
	if info := lookup(doc.Content[0], "info"); info != nil {
		if v := lookup(info, "version"); v != nil {
			v.Value = strings.TrimPrefix(version.String(), "v")
		}
	}
	return &doc, nil
}

// YAML serves the spec as YAML, JSON as JSON.
func YAML() http.Handler { return serve("application/yaml", func(r rendered) []byte { return r.yaml }) }

func JSON() http.Handler { return serve("application/json", func(r rendered) []byte { return r.json }) }

type rendered struct{ yaml, json []byte }

// render runs once. A spec that does not parse is a build mistake, which the
// tests catch, so the server only answers 500 for it rather than not start.
var render = sync.OnceValues(func() (rendered, error) {
	doc, err := Spec()
	if err != nil {
		return rendered{}, fmt.Errorf("openapi.yaml: %w", err)
	}
	var y bytes.Buffer
	enc := yaml.NewEncoder(&y)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return rendered{}, err
	}
	var j bytes.Buffer
	if err := writeJSON(&j, doc.Content[0]); err != nil {
		return rendered{}, err
	}
	return rendered{yaml: y.Bytes(), json: j.Bytes()}, nil
})

func serve(contentType string, pick func(rendered) []byte) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		r, err := render()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", contentType)
		// Anyone may generate a client from it, from a page on another origin
		// too. It holds nothing that is not in the source.
		w.Header().Set("Access-Control-Allow-Origin", "*")
		_, _ = w.Write(pick(r))
	})
}

// lookup returns the value under key in a mapping node, or nil.
func lookup(m *yaml.Node, key string) *yaml.Node {
	if m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// writeJSON writes a node as JSON. It walks the node rather than a decoded map
// so the keys stay in the order they are written, with openapi and info first.
func writeJSON(buf *bytes.Buffer, n *yaml.Node) error {
	switch n.Kind {
	case yaml.AliasNode:
		return writeJSON(buf, n.Alias)
	case yaml.MappingNode:
		buf.WriteByte('{')
		for i := 0; i+1 < len(n.Content); i += 2 {
			if i > 0 {
				buf.WriteByte(',')
			}
			key, _ := json.Marshal(n.Content[i].Value)
			buf.Write(key)
			buf.WriteByte(':')
			if err := writeJSON(buf, n.Content[i+1]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	case yaml.SequenceNode:
		buf.WriteByte('[')
		for i, c := range n.Content {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeJSON(buf, c); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case yaml.ScalarNode:
		var v any
		if err := n.Decode(&v); err != nil {
			return err
		}
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Errorf("line %d: %w", n.Line, err)
		}
		buf.Write(b)
	default:
		return fmt.Errorf("line %d: unexpected YAML node kind %d", n.Line, n.Kind)
	}
	return nil
}
