package main

import "fmt"

// MainKeyboard возвращает клавиатуру с командами.
// Формат соответствует VK API Keyboard (persistent).
func MainKeyboard() map[string]interface{} {
	return map[string]interface{}{
		"one_time": false,
		"persistent": true,
		"buttons": [][]map[string]interface{}{
			{
				{
					"action": map[string]interface{}{
						"type": "text",
						"label": "/status",
					},
					"color": "primary",
				},
				{
					"action": map[string]interface{}{
						"type": "text",
						"label": "/list",
					},
					"color": "primary",
				},
				{
					"action": map[string]interface{}{
						"type": "text",
						"label": "/add",
					},
					"color": "positive",
				},
			},
			{
				{
					"action": map[string]interface{}{
						"type": "text",
						"label": "/help",
					},
					"color": "secondary",
				},
				{
					"action": map[string]interface{}{
						"type": "text",
						"label": "/status all",
					},
					"color": "secondary",
				},
			},
		},
	}
}

// StatusKeyboard возвращает клавиатуру с опциями для активного торрента.
func StatusKeyboard(id int) map[string]interface{} {
	return map[string]interface{}{
		"one_time": true,
		"buttons": [][]map[string]interface{}{
			{
				{
					"action": map[string]interface{}{
						"type": "text",
						"label": fmt.Sprintf("/pause %d", id),
					},
					"color": "negative",
				},
				{
					"action": map[string]interface{}{
						"type": "text",
						"label": fmt.Sprintf("/resume %d", id),
					},
					"color": "positive",
				},
				{
					"action": map[string]interface{}{
						"type": "text",
						"label": fmt.Sprintf("/remove %d", id),
					},
					"color": "negative",
				},
			},
		},
	}
}
