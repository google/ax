# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     https://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

import pytest
from fastapi.testclient import TestClient
from unittest.mock import AsyncMock, MagicMock, patch
import json

from python.antigravity.harness_server import app, hydrate_ax_history_to_steps
from google.antigravity.types import Step, StepType, StepSource, StepTarget, StepStatus, Text, Thought
from proto import ax_pb2

client = TestClient(app)

def test_hydrate_ax_history_to_steps():
    # Create mock AX Message protobuf objects
    msg = ax_pb2.Message()
    msg.role = "user"
    msg.content.text.text = "Hi"
    
    steps = hydrate_ax_history_to_steps([msg])
    
    assert len(steps) == 1
    assert steps[0].source == StepSource.USER
    assert steps[0].content == "Hi"
    assert steps[0].is_complete_response is True

@patch("python.antigravity.harness_server.Agent")
def test_websocket_endpoint_success(mock_agent_class):
    # 1. Setup mocks for Agent, Conversation, and ChatResponse
    mock_agent = MagicMock()
    mock_agent_class.return_value = mock_agent
    
    # Mock context manager methods
    mock_agent.__aenter__ = AsyncMock(return_value=mock_agent)
    mock_agent.__aexit__ = AsyncMock(return_value=None)
    
    mock_conversation = MagicMock()
    mock_agent.conversation = mock_conversation
    mock_conversation._steps = []
    
    # Mock response stream chunks
    async def mock_chunks():
        yield Text(step_index=0, text="Hello ")
        yield Text(step_index=1, text="world!")
        
    mock_chat_response = MagicMock()
    mock_chat_response.chunks = mock_chunks()
    mock_conversation.chat = AsyncMock(return_value=mock_chat_response)
    
    # Load a dummy config globally to pass server validation
    import python.antigravity.harness_server as server
    server.loaded_config = MagicMock()
    
    # 2. Build start payload
    start_payload = {
        "conversation_id": "conv-123",
        "exec_id": "exec-456",
        "messages": [
            # Raw protobuf JSON message
            {
                "role": "user",
                "content": {
                    "text": {"text": "Hi"}
                }
            }
        ]
    }
    
    # 3. Run WebSocket test client
    with client.websocket_connect("/ws") as websocket:
        # Send start payload
        websocket.send_text(json.dumps(start_payload))
        
        # Receive streamed text chunks
        resp1 = websocket.receive_json()
        assert resp1["type"] == "text"
        assert resp1["content"] == "Hello "
        
        resp2 = websocket.receive_json()
        assert resp2["type"] == "text"
        assert resp2["content"] == "world!"
        
        # Receive complete
        resp3 = websocket.receive_json()
        assert resp3["type"] == "complete"
        
    # Verify mocks
    mock_conversation.chat.assert_called_once_with("Hi")

@patch("python.antigravity.harness_server.Agent")
def test_websocket_endpoint_error(mock_agent_class):
    mock_agent = MagicMock()
    mock_agent_class.return_value = mock_agent
    mock_agent.__aenter__ = AsyncMock(return_value=mock_agent)
    mock_agent.__aexit__ = AsyncMock(return_value=None)
    
    mock_conversation = MagicMock()
    mock_agent.conversation = mock_conversation
    mock_conversation._steps = []
    
    # Mock chat to throw an exception
    mock_conversation.chat = AsyncMock(side_effect=RuntimeError("Gemini connection timeout"))
    
    import python.antigravity.harness_server as server
    server.loaded_config = MagicMock()
    
    start_payload = {
        "conversation_id": "conv-123",
        "exec_id": "exec-456",
        "messages": [{"role": "user", "content": {"text": {"text": "Hi"}}}]
    }
    
    with client.websocket_connect("/ws") as websocket:
        websocket.send_text(json.dumps(start_payload))
        
        # Expect error frame
        resp = websocket.receive_json()
        assert resp["type"] == "error"
        assert "Gemini connection timeout" in resp["error"]
