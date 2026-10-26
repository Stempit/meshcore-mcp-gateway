package xiaozhi

// ToolDefinitions returns the MCP tool descriptors advertised in tools/list.
// Descriptions are written for the LLM: they tell it when to use each tool
// and how to phrase results for a voice user.
func ToolDefinitions() []map[string]any {
	return []map[string]any{
		{
			"name": "mesh_get_new_messages",
			"description": "Read new unread messages received over the MeshCore LoRa mesh radio since the previous call. " +
				"Returns a short numbered list of private (dm) and channel messages. " +
				"Call repeatedly to page through a large batch. Use before replying to see the conversation.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			"name": "mesh_send_message",
			"description": "Send a text message over the MeshCore LoRa mesh radio. " +
				"Radio delivery confirmation may take up to 30 seconds, so the call can be slow. " +
				"If unsure about the exact recipient name, call mesh_search_contacts or mesh_list_channels first.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"type": map[string]any{
						"type":        "string",
						"enum":        []string{"dm", "channel"},
						"description": `"dm" for a private message to one node, "channel" for a public channel`,
					},
					"to": map[string]any{
						"type":        "string",
						"description": "Recipient: contact name (or hex public-key prefix) for dm; channel name (or numeric index) for channel",
					},
					"text": map[string]any{
						"type":        "string",
						"description": "Message text to send",
					},
					"ack": map[string]any{
						"type":        "boolean",
						"description": "Wait for end-to-end delivery confirmation (slower). Default false.",
					},
				},
				"required": []string{"type", "to", "text"},
			},
		},
		{
			"name": "mesh_search_contacts",
			"description": "Search mesh contacts by name fragment — case, emoji and punctuation are ignored, and a hex public-key prefix works too. " +
				"Returns all matching names. Use it to find the exact contact name before sending a private message, " +
				"especially when the contact list is large.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "Name fragment to search for (at least 2 characters)",
					},
				},
				"required": []string{"query"},
			},
		},
		{
			"name":        "mesh_list_contacts",
			"description": "List the first known mesh contact names. Only for small contact lists — with many contacts use mesh_search_contacts to find one by name.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			"name":        "mesh_list_channels",
			"description": "List the configured mesh channels with their indices. Use it to resolve a channel name before sending to a channel.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
	}
}
