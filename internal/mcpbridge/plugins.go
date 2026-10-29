package mcpbridge

import (
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
)

func pluginsListTool() Tool {
	t := NewTool("plugins_list", "List built-in plugins, enablement and views, without settings.", "/plugins")
	t.schema = map[string]any{"type": "object", "properties": map[string]any{}}
	t.project = func(body []byte) ([]byte, error) {
		var items []map[string]json.RawMessage
		if err := json.Unmarshal(body, &items); err != nil {
			return nil, err
		}
		for _, item := range items {
			delete(item, "settings")
			delete(item, "settings_schema")
		}
		return json.Marshal(items)
	}
	return t
}

var pluginID = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)
var pluginView = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

func pluginViewTool() Tool {
	t := NewTool("plugin_view", "Read one enabled plugin's declarative view.", "")
	t.schema = map[string]any{"type": "object", "required": []string{"plugin", "view"}, "properties": map[string]any{
		"plugin": map[string]any{"type": "string"}, "view": map[string]any{"type": "string"},
		"params": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}},
	}}
	t.Path = func(args map[string]any) (string, url.Values, error) {
		id, ok := args["plugin"].(string)
		if !ok || !pluginID.MatchString(id) {
			return "", nil, errors.New("plugin must be a valid plugin ID")
		}
		view, ok := args["view"].(string)
		if !ok || !pluginView.MatchString(view) {
			return "", nil, errors.New("view must be a valid view name")
		}
		q := url.Values{}
		if p, exists := args["params"]; exists {
			values, ok := p.(map[string]any)
			if !ok {
				return "", nil, errors.New("params must be an object of strings")
			}
			for key, value := range values {
				s, ok := value.(string)
				if !ok {
					return "", nil, errors.New("params must be an object of strings")
				}
				q.Set(key, s)
			}
		}
		return "/plugins/" + url.PathEscape(id) + "/views/" + url.PathEscape(view), q, nil
	}
	return t
}
