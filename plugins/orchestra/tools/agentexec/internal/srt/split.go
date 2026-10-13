package srt

import "bytes"

// DebugPrefix starts every srt debug line.
var DebugPrefix = []byte("[SandboxDebug]")

// StderrSplit separates srt's debug lines from the executor's own stderr
// (they share one stream). Complete lines starting with [SandboxDebug] go
// to the run's log; everything else is returned by Feed/Flush. srt logs its
// network config as pretty-printed JSON whose first line alone carries the
// prefix (`[SandboxDebug] {`): the lines up to the closing bracket at
// column 0 belong to it.
type StderrSplit struct {
	run       *Run
	buf       []byte
	closeLine []byte // nil: not inside a multi-line debug record
}

// NewStderrSplit splits for run.
func NewStderrSplit(run *Run) *StderrSplit { return &StderrSplit{run: run} }

// Feed takes a chunk of stderr and returns the executor's part of every
// complete line in it.
func (s *StderrSplit) Feed(chunk []byte) []byte {
	s.buf = append(s.buf, chunk...)
	var keep []byte
	for {
		nl := bytes.IndexByte(s.buf, '\n')
		if nl < 0 {
			break
		}
		line := append([]byte(nil), s.buf[:nl+1]...)
		s.buf = s.buf[nl+1:]
		keep = append(keep, s.route(line)...)
	}
	return keep
}

// Flush routes what is left (a last line without a newline).
func (s *StderrSplit) Flush() []byte {
	rest := s.buf
	s.buf = nil
	if len(rest) == 0 {
		return nil
	}
	return s.route(rest)
}

func (s *StderrSplit) route(line []byte) []byte {
	text := bytes.TrimRight(line, "\r\n")
	if s.closeLine != nil {
		if bytes.Equal(text, s.closeLine) {
			s.closeLine = nil
		}
		s.run.Log(append(append(append([]byte(nil), DebugPrefix...), ' '), append(text, '\n')...))
		return nil
	}
	if bytes.HasPrefix(line, DebugPrefix) {
		switch string(bytes.TrimSpace(text[len(DebugPrefix):])) {
		case "{":
			s.closeLine = []byte("}")
		case "[":
			s.closeLine = []byte("]")
		}
		s.run.Log(append(append([]byte(nil), text...), '\n'))
		return nil
	}
	return line
}
