package handler

import "time"

//Compilation is a compilation job which is put in a
//compile queue and executed when the server is free
type Compilation struct {
	//server options
	ID          string    `json:"id"`
	Queued      bool      `json:"queued"`
	StartedAt   time.Time `json:"startedAt"`
	Completed   bool      `json:"completed"`
	CompletedAt time.Time `json:"completedAt"`
	Error       string    `json:"error,omitempty"`
	Releaser    string    `json:"releaser,omitempty"`
	OSArch      []string  `json:"osarch"`
	Files       []string  `json:"files"`
	//TODO user inline main file
	MainContents string `json:"-"`
	//user external package
	Package   string `json:"name"`
	Commitish string `json:"commitish"`
	CommitVar string `json:"commitVar"`
	LabelVar  string `json:"labelVar"`
	Label     string `json:"label"`
	//user compile options
	CGO        bool              `json:"cgo"`
	Shrink     bool              `json:"shrink"` //ldflag -s -w
	GoGet      bool              `json:"goGet"`
	Version    string            `json:"version"`
	VersionVar string            `json:"versionVar"`
	Platforms  Platforms         `json:"platforms"`
	Targets    []string          `json:"targets"`
	Variables  map[string]string `json:"variables"`
	Env        map[string]string `json:"env"`

	//[SWH|+]
	GoGenerate bool   `json:"goGenerate"`
	Tags       string `json:"tags"`
}
