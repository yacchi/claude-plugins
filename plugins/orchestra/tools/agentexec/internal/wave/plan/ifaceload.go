package plan

import (
	"os"
	"strconv"
	"strings"
)

// SpecBodyAllowance is the lines a spec needs besides its pasted interfaces.
const SpecBodyAllowance = 60

// CheckInterfaceLoad returns an *Error naming every package whose listed
// interfaces' lines (countLines of each file; an unreadable file counts 0)
// plus SpecBodyAllowance exceed maxSpecLines (150); nil when none does.
func CheckInterfaceLoad(p *Plan) error {
	var findings []string
	for _, pkg := range p.Packages {
		total := 0
		parts := make([]string, 0, len(pkg.Interfaces))
		for _, id := range pkg.Interfaces {
			lines := 0
			if iface, ok := p.Interface(id); ok {
				if data, err := os.ReadFile(iface.Path); err == nil {
					lines = countLines(string(data))
				}
			}
			parts = append(parts, id+" "+strconv.Itoa(lines))
			total += lines
		}
		if total+SpecBodyAllowance <= maxSpecLines {
			continue
		}
		findings = append(findings, "package "+pkg.ID+" lists "+strconv.Itoa(total)+
			" interface lines ("+strings.Join(parts, ", ")+"); with a "+strconv.Itoa(SpecBodyAllowance)+
			"-line body its spec passes the "+strconv.Itoa(maxSpecLines)+
			"-line limit: list only the interfaces the package produces, and quote in its brief the lines it consumes")
	}
	if len(findings) == 0 {
		return nil
	}
	return &Error{Msg: strings.Join(findings, "; ")}
}
