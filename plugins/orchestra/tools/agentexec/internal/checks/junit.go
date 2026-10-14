package checks

import (
	"encoding/xml"
	"io"
	"os"
	"strings"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
)

// xmlNode is the part of an element tree the JUnit reader needs; text is the
// text before the first child, like ElementTree's .text.
type xmlNode struct {
	name     string
	attrs    map[string]string
	text     string
	hasText  bool
	children []*xmlNode
}

func parseXML(r io.Reader) (*xmlNode, error) {
	dec := xml.NewDecoder(r)
	dec.CharsetReader = func(_ string, in io.Reader) (io.Reader, error) { return in, nil }
	var stack []*xmlNode
	var root *xmlNode
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			n := &xmlNode{attrs: map[string]string{}}
			if t.Name.Space == "" {
				n.name = t.Name.Local
			} else {
				n.name = "{" + t.Name.Space + "}" + t.Name.Local
			}
			for _, a := range t.Attr {
				if a.Name.Space == "" {
					n.attrs[a.Name.Local] = a.Value
				}
			}
			if len(stack) > 0 {
				parent := stack[len(stack)-1]
				parent.children = append(parent.children, n)
			} else if root == nil {
				root = n
			}
			stack = append(stack, n)
		case xml.EndElement:
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		case xml.CharData:
			if len(stack) > 0 {
				top := stack[len(stack)-1]
				if len(top.children) == 0 {
					top.text += string(t)
					top.hasText = true
				}
			}
		}
	}
	if root == nil {
		return nil, io.ErrUnexpectedEOF
	}
	return root, nil
}

func (n *xmlNode) child(name string) *xmlNode {
	for _, c := range n.children {
		if c.name == name {
			return c
		}
	}
	return nil
}

func (n *xmlNode) walk(fn func(*xmlNode)) {
	fn(n)
	for _, c := range n.children {
		c.walk(fn)
	}
}

// junitFailureLines is "classname > name: <first line of message>" per
// failing/erroring testcase, or ok=false when path is missing or not JUnit
// XML.
func junitFailureLines(path string) (lines []string, ok bool) {
	if !isFile(path) {
		return nil, false
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	root, err := parseXML(f)
	if err != nil {
		return nil, false
	}
	root.walk(func(n *xmlNode) {
		if n.name != "testcase" {
			return
		}
		node := n.child("failure")
		if node == nil {
			node = n.child("error")
		}
		if node == nil {
			return
		}
		message := node.attrs["message"]
		if message == "" {
			message = node.text
		}
		message = strings.TrimSpace(message)
		first := ""
		if ls := fsutil.SplitLines(message); len(ls) > 0 {
			first = ls[0]
		}
		lines = append(lines, n.attrs["classname"]+" > "+n.attrs["name"]+": "+first)
	})
	return lines, true
}

func buildExcerpt(junitPath, combined string, limit int) string {
	if junitPath != "" {
		if lines, ok := junitFailureLines(junitPath); ok && len(lines) > 0 {
			header := strings.Join(lines, "\n")
			hr := []rune(header)
			if len(hr) >= limit {
				return string(hr[:limit])
			}
			rest := excerptLimit(combined, limit-len(hr)-1)
			out := header
			if rest != "" {
				out += "\n" + rest
			}
			or := []rune(out)
			if len(or) > limit {
				or = or[:limit]
			}
			return string(or)
		}
	}
	return excerptLimit(combined, limit)
}
