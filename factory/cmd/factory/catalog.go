package main

import (
	"encoding/json"
	"os"
	"sort"

	"apifactory/factory/ir"
	"apifactory/factory/spec"
)

// CatalogPath: what each API is, how to call it and the person-written OK cases (request → expected response under the
// case's own fixtures). Read by the delivery board ("try it" on a delivery); generated with the code, so the deployed
// commit's catalog matches the deployed API.
const CatalogPath = "gen/api-catalog.json"

type catalogCase struct {
	Name     string         `json:"name"`
	Request  map[string]any `json:"request"`
	Response map[string]any `json:"response"`
}

type catalogAPI struct {
	ID      string        `json:"id"`
	Method  string        `json:"method"`
	Summary string        `json:"summary"`
	Path    string        `json:"path"` // Connect: POST <base><path>, JSON body
	Pattern string        `json:"pattern"`
	Cases   []catalogCase `json:"cases"`
}

func writeCatalog(plan *ir.Plan, specs []*spec.Spec) error {
	byID := map[string]*spec.Spec{}
	for _, s := range specs {
		byID[s.ID()] = s
	}
	var out []catalogAPI
	for _, a := range plan.APIs {
		c := catalogAPI{ID: a.ID, Method: a.Method, Summary: a.Summary, Pattern: a.Pattern,
			Path: "/" + a.ProtoPackage + "." + a.Service + "/" + a.Method, Cases: []catalogCase{}}
		if s := byID[a.ID]; s != nil {
			for _, k := range s.Acceptance.Cases {
				if k.Expect.Code == "OK" && len(k.Steps) == 0 && len(c.Cases) < 3 {
					req := k.Request
					if req == nil {
						req = map[string]any{}
					}
					c.Cases = append(c.Cases, catalogCase{Name: k.Name, Request: req, Response: k.Expect.Response})
				}
			}
		}
		if len(c.Cases) == 0 && a.BaseRequest != nil {
			c.Cases = append(c.Cases, catalogCase{Name: "示例请求", Request: a.BaseRequest})
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(CatalogPath, append(b, '\n'), 0o644)
}
