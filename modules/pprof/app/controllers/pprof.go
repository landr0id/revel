package controllers

import (
	"github.com/robfig/revel"
	"net/http/pprof"
)

type Pprof struct {
	*revel.Controller
}

const (
	index   = 0
	profile = 1
	symbol  = 2
	cmdline = 3
)

type PprofResponse int

func (r PprofResponse) Apply(req *revel.Request, resp *revel.Response) {
	switch r {
	case profile:
		pprof.Profile(resp.Out, req.Request)
	case symbol:
		pprof.Symbol(resp.Out, req.Request)
	case index:
		pprof.Index(resp.Out, req.Request)
	case cmdline:
		pprof.Cmdline(resp.Out, req.Request)
	}
}

func (c Pprof) Profile() revel.Result {
	return PprofResponse(profile)
}

func (c Pprof) Symbol() revel.Result {
	return PprofResponse(symbol)
}

func (c Pprof) Cmdline() revel.Result {
	return PprofResponse(cmdline)
}

func (c Pprof) Index() revel.Result {
	return PprofResponse(index)
}
