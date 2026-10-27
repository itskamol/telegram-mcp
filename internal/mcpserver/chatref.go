package mcpserver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
)

// ChatRef is a chat argument the way models send it: the numeric id from
// list_chats as a JSON number or a string, "@username", or "me". Accepting
// both number and string matters: tools return ids as numbers, and a
// string-only schema made models fail and retry.
type ChatRef string

// UnmarshalJSON accepts a string or a whole JSON number.
func (c *ChatRef) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*c = ChatRef(strings.TrimSpace(s))
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber() // keep -100... ids exact
	var n json.Number
	if err := dec.Decode(&n); err != nil {
		return fmt.Errorf(`chat must be a chat id, "@username" or "me"`)
	}
	if _, err := n.Int64(); err != nil {
		return fmt.Errorf("chat id %s is not a whole number", n)
	}
	*c = ChatRef(n.String())
	return nil
}

// inputSchema infers the tool input schema for In, typing ChatRef fields
// as string-or-integer. Field descriptions come from jsonschema tags.
func inputSchema[In any]() *jsonschema.Schema {
	s, err := jsonschema.For[In](&jsonschema.ForOptions{
		TypeSchemas: map[reflect.Type]*jsonschema.Schema{
			reflect.TypeFor[ChatRef](): {Types: []string{"integer", "string"}},
		},
	})
	if err != nil {
		panic(fmt.Sprintf("schema for %T: %v", *new(In), err)) // a bug in the input struct
	}
	dropNull(s)
	return s
}

// dropNull removes "null" from type unions. jsonschema makes Go slices
// nullable (["null","array"]), and MCP clients show such a union without
// any type, so the model no longer sees that message_ids is a list of
// integers. A missing optional field already means "not given".
func dropNull(s *jsonschema.Schema) {
	if s == nil {
		return
	}
	if len(s.Types) > 1 {
		s.Types = slices.DeleteFunc(s.Types, func(t string) bool { return t == "null" })
		if len(s.Types) == 1 {
			s.Type, s.Types = s.Types[0], nil
		}
	}
	for _, p := range s.Properties {
		dropNull(p)
	}
	dropNull(s.Items)
}
