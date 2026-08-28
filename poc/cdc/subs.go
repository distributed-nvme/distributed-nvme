package main

import (
	"encoding/json"
	"os"
)

type port struct {
	Traddr  string `json:"traddr"`
	Trsvcid string `json:"trsvcid"`
}

type subsys struct {
	Nqn          string   `json:"nqn"`
	AllowedHosts []string `json:"allowed_hosts"`
	Ports        []port   `json:"ports"`
}

type model struct {
	Subsystems []subsys `json:"subsystems"`
}

func loadModel(path string) (*model, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m model
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func hostAllowed(s *subsys, hostnqn string) bool {
	for _, h := range s.AllowedHosts {
		if h == hostnqn {
			return true
		}
	}
	return false
}
