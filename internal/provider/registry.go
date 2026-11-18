// Package provider routes a model request to the configured providers that
// can serve it, in priority order.
package provider

import (
	"fmt"
	"sort"
	"strings"

	"github.com/972579/open-image-mcp/internal/config"
	"github.com/972579/open-image-mcp/internal/imageapi"
)

// Capability is what a call needs from a model.
type Capability string

// Capabilities a model can declare.
const (
	Generate Capability = "generate"
	Edit     Capability = "edit"
)

// Candidate is one provider able to serve the requested model.
type Candidate struct {
	Provider config.Provider
	Model    config.Model
}

// Target converts a candidate into the image client's request target.
func (c Candidate) Target() imageapi.Target {
	return imageapi.Target{
		Provider:   c.Provider.Name,
		Dialect:    c.Provider.Dialect,
		BaseURL:    c.Provider.BaseURL,
		APIKey:     c.Provider.APIKey,
		Headers:    c.Provider.Headers,
		ExtraBody:  c.Provider.ExtraBody,
		Model:      c.Model.ID,
		ModelExtra: c.Model.Extra,
	}
}

// ModelInfo describes one model for list_models and the GUI.
type ModelInfo struct {
	ID        string   `json:"id"`
	Provider  string   `json:"provider"`
	Generate  bool     `json:"generate"`
	Edit      bool     `json:"edit"`
	Sizes     []string `json:"sizes,omitempty"`
	IsDefault bool     `json:"is_default"`
}

// supports reports whether m declares capability cap.
func supports(m config.Model, cap Capability) bool {
	if cap == Edit {
		return m.Edit
	}
	return m.Generate
}

// Resolve lists the providers that can serve modelID for cap, in config order.
//
// cfg is the effective configuration. modelID may be empty, meaning
// defaults.model, or when that is empty too, the first enabled model with the
// capability. providerHint, when set, restricts the search to that provider;
// a model it does not list is still allowed through (so users can try models
// they have not added yet). With fallback disabled only the first candidate is
// returned. It returns an *imageapi.Error of KindConfig when nothing matches.
func Resolve(cfg *config.Config, modelID, providerHint string, cap Capability) ([]Candidate, error) {
	modelID = strings.TrimSpace(modelID)
	providerHint = strings.TrimSpace(providerHint)
	if modelID == "" {
		modelID = cfg.Defaults.Model
	}

	if providerHint != "" {
		p := cfg.ProviderByName(providerHint)
		if p == nil {
			return nil, configErr(fmt.Sprintf("找不到服务商 %q；已配置：%s", providerHint, strings.Join(providerNames(cfg), ", ")))
		}
		if !p.Enabled {
			return nil, configErr(fmt.Sprintf("服务商 %q 已停用", providerHint))
		}
		if modelID == "" {
			for _, m := range p.Models {
				if supports(m, cap) {
					return []Candidate{{Provider: *p, Model: m}}, nil
				}
			}
			return nil, configErr(fmt.Sprintf("服务商 %q 没有支持%s的模型", providerHint, capLabel(cap)))
		}
		for _, m := range p.Models {
			if m.ID == modelID {
				if !supports(m, cap) {
					return nil, capErr(modelID, p.Name, cap)
				}
				return []Candidate{{Provider: *p, Model: m}}, nil
			}
		}
		return []Candidate{{Provider: *p, Model: config.Model{ID: modelID, Generate: true, Edit: true}}}, nil
	}

	if modelID == "" {
		for _, p := range cfg.Providers {
			if !p.Enabled {
				continue
			}
			for _, m := range p.Models {
				if supports(m, cap) {
					modelID = m.ID
					break
				}
			}
			if modelID != "" {
				break
			}
		}
		if modelID == "" {
			return nil, configErr(fmt.Sprintf("没有任何已启用的服务商配置了支持%s的模型，请先在「服务商」页添加", capLabel(cap)))
		}
	}

	var out []Candidate
	var lacking []string
	for _, pass := range []func(a, b string) bool{func(a, b string) bool { return a == b }, strings.EqualFold} {
		for _, p := range cfg.Providers {
			if !p.Enabled {
				continue
			}
			for _, m := range p.Models {
				if !pass(m.ID, modelID) {
					continue
				}
				if supports(m, cap) {
					out = append(out, Candidate{Provider: p, Model: m})
				} else {
					lacking = append(lacking, p.Name)
				}
			}
		}
		if len(out) > 0 || len(lacking) > 0 {
			break
		}
	}
	if len(out) == 0 {
		if len(lacking) > 0 {
			return nil, capErr(modelID, strings.Join(lacking, ", "), cap)
		}
		return nil, configErr(fmt.Sprintf("没有已启用的服务商提供模型 %q。可用模型：%s", modelID, strings.Join(modelIDs(cfg, cap), ", ")))
	}
	if !cfg.Request.Fallback {
		out = out[:1]
	}
	return out, nil
}

// List returns every model of every enabled provider, optionally filtered.
//
// providerName limits the list to one provider when not empty; cap limits it
// to models with that capability when not empty.
func List(cfg *config.Config, providerName string, cap Capability) []ModelInfo {
	var out []ModelInfo
	for _, p := range cfg.Providers {
		if !p.Enabled || (providerName != "" && p.Name != providerName) {
			continue
		}
		for _, m := range p.Models {
			if cap != "" && !supports(m, cap) {
				continue
			}
			out = append(out, ModelInfo{
				ID: m.ID, Provider: p.Name, Generate: m.Generate, Edit: m.Edit,
				Sizes: m.Sizes, IsDefault: m.ID == cfg.Defaults.Model,
			})
		}
	}
	return out
}

// modelIDs returns the distinct model IDs that support cap, sorted.
func modelIDs(cfg *config.Config, cap Capability) []string {
	seen := map[string]bool{}
	var ids []string
	for _, m := range List(cfg, "", cap) {
		if !seen[m.ID] {
			seen[m.ID] = true
			ids = append(ids, m.ID)
		}
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		return []string{"（无）"}
	}
	return ids
}

// providerNames returns the configured provider names in order.
func providerNames(cfg *config.Config) []string {
	names := make([]string, 0, len(cfg.Providers))
	for _, p := range cfg.Providers {
		names = append(names, p.Name)
	}
	if len(names) == 0 {
		return []string{"（无）"}
	}
	return names
}

// capLabel names a capability in Chinese.
func capLabel(cap Capability) string {
	if cap == Edit {
		return "编辑"
	}
	return "生成"
}

// configErr wraps a routing problem as a KindConfig error.
func configErr(msg string) error {
	return &imageapi.Error{Kind: imageapi.KindConfig, Message: msg}
}

// capErr reports that a model is configured but not for the needed capability.
func capErr(model, providers string, cap Capability) error {
	return &imageapi.Error{
		Kind:    imageapi.KindModel,
		Message: fmt.Sprintf("模型 %s 在服务商 %s 中未声明支持%s", model, providers, capLabel(cap)),
		Hint:    fmt.Sprintf("如确实支持，请在「服务商」页勾选该模型的「%s」", capLabel(cap)),
	}
}
