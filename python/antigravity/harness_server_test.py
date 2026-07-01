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

import asyncio
import pytest
import grpc
from python.proto import ax_pb2, ax_pb2_grpc, content_pb2
from python.antigravity.harness_server import AntigravityHarnessServiceServicer
from google.antigravity import LocalAgentConfig

@pytest.fixture
def mock_config(monkeypatch):
    monkeypatch.setenv("GEMINI_API_KEY", "mock-api-key")
    return LocalAgentConfig(system_instructions="Test instructions")

def test_grpc_connect_success(mock_config, monkeypatch):
    async def _run():
        # 1. Start temporary local gRPC server on random open port
        server = grpc.aio.server()
        servicer = AntigravityHarnessServiceServicer(mock_config)
        ax_pb2_grpc.add_HarnessServiceServicer_to_server(servicer, server)
        port = server.add_insecure_port("localhost:0")
        await server.start()
        
        # 2. Connect async stub channel
        addr = f"localhost:{port}"
        async with grpc.aio.insecure_channel(addr) as channel:
            stub = ax_pb2_grpc.HarnessServiceStub(channel)
            
            # Mock the underlying Antigravity SDK class calls
            class MockConversation:
                def __init__(self):
                    self._steps = []
                async def chat(self, text):
                    class MockResponse:
                        def __init__(self):
                            self.chunks = self._chunk_generator()
                        async def _chunk_generator(self):
                            from google.antigravity.types import Text, Thought
                            yield Thought(text="Thinking details", step_index=0)
                            yield Text(text="Hello human", step_index=0)
                    return MockResponse()
                    
            class MockAgent:
                def __init__(self, config):
                    self.conversation = MockConversation()
                async def __aenter__(self):
                    return self
                async def __aexit__(self, exc_type, exc, tb):
                    pass
                    
            monkeypatch.setattr("python.antigravity.harness_server.Agent", MockAgent)
            
            # 3. Construct and fire a HarnessRequest{start} over the bidi stream
            start_payload = ax_pb2.HarnessStart(
                messages=[
                    ax_pb2.Message(role="user", content=content_pb2.Content(text=content_pb2.TextContent(text="Hi")))
                ]
            )
            req = ax_pb2.HarnessRequest(
                conversation_id="conv-test",
                harness_id="antigravity",
                start=start_payload
            )
            
            async def request_iter():
                yield req

            responses = []
            async for resp in stub.Connect(request_iter()):
                responses.append(resp)
                
            # 4. Assert outputs are correctly mapped and completed
            assert len(responses) == 3 # Thought + Text + End
            assert responses[0].outputs.messages[0].content.thought.summary[0].text.text == "Thinking details"
            assert responses[1].outputs.messages[0].content.text.text == "Hello human"
            assert responses[2].WhichOneof('type') == 'end'
            assert responses[2].end.state == ax_pb2.STATE_COMPLETED
            
        await server.stop(0)

    asyncio.run(_run())


def test_grpc_connect_agent_reused(mock_config, monkeypatch):
    async def _run():
        server = grpc.aio.server()
        servicer = AntigravityHarnessServiceServicer(mock_config)
        ax_pb2_grpc.add_HarnessServiceServicer_to_server(servicer, server)
        port = server.add_insecure_port("localhost:0")
        await server.start()
        
        addr = f"localhost:{port}"
        async with grpc.aio.insecure_channel(addr) as channel:
            stub = ax_pb2_grpc.HarnessServiceStub(channel)
            
            class MockConversation:
                def __init__(self):
                    self._steps = []
                async def chat(self, text):
                    class MockResponse:
                        def __init__(self):
                            self.chunks = self._chunk_generator()
                        async def _chunk_generator(self):
                            from google.antigravity.types import Text
                            yield Text(text="Response", step_index=0)
                    return MockResponse()
                    
            agent_instances = []
            class MockAgent:
                def __init__(self, config):
                    self.conversation = MockConversation()
                    self.closed = False
                    agent_instances.append(self)
                async def __aenter__(self):
                    return self
                async def __aexit__(self, exc_type, exc, tb):
                    self.closed = True
                    
            monkeypatch.setattr("python.antigravity.harness_server.Agent", MockAgent)
            
            # Fire first turn for conv-1
            req1 = ax_pb2.HarnessRequest(
                conversation_id="conv-1",
                harness_id="antigravity",
                start=ax_pb2.HarnessStart(
                    messages=[ax_pb2.Message(role="user", content=content_pb2.Content(text=content_pb2.TextContent(text="Hi")))]
                )
            )
            async def req_iter1():
                yield req1
            async for _ in stub.Connect(req_iter1()):
                pass
            
            # Fire second turn for same conv-1
            req2 = ax_pb2.HarnessRequest(
                conversation_id="conv-1",
                harness_id="antigravity",
                start=ax_pb2.HarnessStart(
                    messages=[ax_pb2.Message(role="user", content=content_pb2.Content(text=content_pb2.TextContent(text="Hi again")))]
                )
            )
            async def req_iter2():
                yield req2
            async for _ in stub.Connect(req_iter2()):
                pass
                
            # Fire third turn for a different conv-2
            req3 = ax_pb2.HarnessRequest(
                conversation_id="conv-2",
                harness_id="antigravity",
                start=ax_pb2.HarnessStart(
                    messages=[ax_pb2.Message(role="user", content=content_pb2.Content(text=content_pb2.TextContent(text="New conv")))]
                )
            )
            async def req_iter3():
                yield req3
            async for _ in stub.Connect(req_iter3()):
                pass
                
            # Verify only 2 agents were instantiated (reused the first one)
            assert len(agent_instances) == 2
            
            # Verify cleanup closes all agents
            await servicer.cleanup()
            assert all(a.closed for a in agent_instances)
            
        await server.stop(0)

    asyncio.run(_run())


def test_health_check():
    async def _run():
        from grpc_health.v1 import health, health_pb2, health_pb2_grpc

        cfg = LocalAgentConfig(system_instructions="health-check stub")
        server = grpc.aio.server()
        ax_pb2_grpc.add_HarnessServiceServicer_to_server(AntigravityHarnessServiceServicer(cfg), server)
        health_servicer = health.aio.HealthServicer()
        health_pb2_grpc.add_HealthServicer_to_server(health_servicer, server)
        await health_servicer.set("", health_pb2.HealthCheckResponse.SERVING)
        port = server.add_insecure_port("localhost:0")
        await server.start()
        try:
            async with grpc.aio.insecure_channel(f"localhost:{port}") as channel:
                stub = health_pb2_grpc.HealthStub(channel)
                resp = await stub.Check(health_pb2.HealthCheckRequest(service=""))
                assert resp.status == health_pb2.HealthCheckResponse.SERVING
        finally:
            await server.stop(0)

    asyncio.run(_run())


def test_has_credentials_missing(monkeypatch):
    """Returns False when neither env nor config provides credentials."""
    from python.antigravity.harness_server import _has_credentials

    monkeypatch.delenv("GEMINI_API_KEY", raising=False)
    monkeypatch.delenv("GOOGLE_GENAI_USE_VERTEXAI", raising=False)
    monkeypatch.delenv("GOOGLE_GENAI_USE_ENTERPRISE", raising=False)

    cfg = LocalAgentConfig(system_instructions="test")
    assert _has_credentials(cfg) is False


def test_has_credentials_vertex_requires_project_and_location(monkeypatch):
    """vertex=True alone is not enough; AGY requires project+location too."""
    from python.antigravity.harness_server import _has_credentials

    monkeypatch.delenv("GEMINI_API_KEY", raising=False)

    cfg_vertex_only = LocalAgentConfig(system_instructions="test", vertex=True)
    assert _has_credentials(cfg_vertex_only) is False

    cfg_vertex_proj_only = LocalAgentConfig(system_instructions="test", vertex=True, project="p")
    assert _has_credentials(cfg_vertex_proj_only) is False

    cfg_vertex_loc_only = LocalAgentConfig(system_instructions="test", vertex=True, location="us-central1")
    assert _has_credentials(cfg_vertex_loc_only) is False

    cfg_vertex_full = LocalAgentConfig(system_instructions="test", vertex=True, project="p", location="us-central1")
    assert _has_credentials(cfg_vertex_full) is True


def test_has_credentials_vertex_express_mode(monkeypatch):
    """vertex=True + api_key (Express Mode) is accepted even without project/location."""
    from python.antigravity.harness_server import _has_credentials

    monkeypatch.delenv("GEMINI_API_KEY", raising=False)

    cfg = LocalAgentConfig(system_instructions="test", vertex=True, api_key="express-key")
    assert _has_credentials(cfg) is True


def test_grpc_connect_programmatic_credentials(monkeypatch):
    monkeypatch.delenv("GEMINI_API_KEY", raising=False)
    monkeypatch.delenv("GOOGLE_API_KEY", raising=False)
    monkeypatch.delenv("GOOGLE_GENAI_USE_VERTEXAI", raising=False)
    monkeypatch.delenv("GOOGLE_GENAI_USE_ENTERPRISE", raising=False)

    # Config with API key programmatically set
    cfg = LocalAgentConfig(system_instructions="Test instructions", api_key="mock-config-api-key")

    async def _run():
        server = grpc.aio.server()
        servicer = AntigravityHarnessServiceServicer(cfg)
        ax_pb2_grpc.add_HarnessServiceServicer_to_server(servicer, server)
        port = server.add_insecure_port("localhost:0")
        await server.start()
        
        addr = f"localhost:{port}"
        async with grpc.aio.insecure_channel(addr) as channel:
            stub = ax_pb2_grpc.HarnessServiceStub(channel)
            
            # Mock Agent so we can test programmatic config logic passes
            class MockConversation:
                def __init__(self):
                    self._steps = []
                async def chat(self, text):
                    class MockResponse:
                        def __init__(self):
                            self.chunks = self._chunk_generator()
                        async def _chunk_generator(self):
                            from google.antigravity.types import Text
                            yield Text(text="Passed check", step_index=0)
                    return MockResponse()
                    
            class MockAgent:
                def __init__(self, config):
                    self.conversation = MockConversation()
                async def __aenter__(self):
                    return self
                async def __aexit__(self, exc_type, exc, tb):
                    pass
            monkeypatch.setattr("python.antigravity.harness_server.Agent", MockAgent)

            start_payload = ax_pb2.HarnessStart(
                messages=[
                    ax_pb2.Message(role="user", content=content_pb2.Content(text=content_pb2.TextContent(text="Hi")))
                ]
            )
            req = ax_pb2.HarnessRequest(
                conversation_id="conv-test-prog",
                harness_id="antigravity",
                start=start_payload
            )
            
            async def request_iter():
                yield req

            responses = []
            async for resp in stub.Connect(request_iter()):
                responses.append(resp)
                
            assert len(responses) == 2 # Text + End
            assert responses[0].outputs.messages[0].content.text.text == "Passed check"
            assert responses[1].end.state == ax_pb2.STATE_COMPLETED
            
        await server.stop(0)

    asyncio.run(_run())


def test_enhance_config_from_env(monkeypatch, tmp_path):
    from python.antigravity.harness_server import _enhance_config_from_env
    from google.antigravity import LocalAgentConfig
    import os
    
    # Create a mock skills dir
    skills_dir = tmp_path / "skills"
    skills_dir.mkdir()
    
    cfg = LocalAgentConfig(system_instructions="test")
    
    # Test: Using SKILLS_DIR env var
    monkeypatch.setenv("SKILLS_DIR", str(skills_dir))
    _enhance_config_from_env(cfg)
    assert str(skills_dir) in cfg.skills_paths


def test_grpc_connect_buffering(mock_config, monkeypatch):
    async def _run():
        server = grpc.aio.server()
        servicer = AntigravityHarnessServiceServicer(mock_config)
        ax_pb2_grpc.add_HarnessServiceServicer_to_server(servicer, server)
        port = server.add_insecure_port("localhost:0")
        await server.start()
        
        addr = f"localhost:{port}"
        async with grpc.aio.insecure_channel(addr) as channel:
            stub = ax_pb2_grpc.HarnessServiceStub(channel)
            
            class MockConversation:
                def __init__(self):
                    self._steps = []
                async def chat(self, text):
                    class MockResponse:
                        def __init__(self):
                            self.chunks = self._chunk_generator()
                        async def _chunk_generator(self):
                            from google.antigravity.types import Text, Thought, ToolCall
                            yield Thought(text="Think1", step_index=0)
                            yield Thought(text=" Think2", step_index=0)
                            yield ToolCall(name="tool1", args={}, id="call1")
                            yield Text(text="Hello", step_index=0)
                            yield Text(text=" human", step_index=0)
                    return MockResponse()
                    
            class MockAgent:
                def __init__(self, config):
                    self.conversation = MockConversation()
                async def __aenter__(self):
                    return self
                async def __aexit__(self, exc_type, exc, tb):
                    pass
            monkeypatch.setattr("python.antigravity.harness_server.Agent", MockAgent)

            start_payload = ax_pb2.HarnessStart(
                messages=[
                    ax_pb2.Message(role="user", content=content_pb2.Content(text=content_pb2.TextContent(text="Hi")))
                ]
            )
            req = ax_pb2.HarnessRequest(
                conversation_id="conv-test-buffer",
                harness_id="antigravity",
                start=start_payload
            )
            
            async def request_iter():
                yield req

            responses = []
            async for resp in stub.Connect(request_iter()):
                responses.append(resp)
                
            # Responses should be:
            # 1. Thought ("Think1 Think2") - flushed when ToolCall is encountered
            # 2. ToolCall ("tool1") - processed immediately
            # 3. Text ("Hello human") - flushed at the end
            # 4. End frame
            assert len(responses) == 4
            
            # Assert 1st response: Thought summary text is "Think1 Think2"
            assert responses[0].outputs.messages[0].content.WhichOneof('type') == 'thought'
            assert responses[0].outputs.messages[0].content.thought.summary[0].text.text == "Think1 Think2"
            
            # Assert 2nd response: ToolCall name is "tool1"
            assert responses[1].outputs.messages[0].content.WhichOneof('type') == 'tool_call'
            assert responses[1].outputs.messages[0].content.tool_call.function_call.name == "tool1"
            
            # Assert 3rd response: Text content is "Hello human"
            assert responses[2].outputs.messages[0].content.WhichOneof('type') == 'text'
            assert responses[2].outputs.messages[0].content.text.text == "Hello human"
            
            # Assert 4th response: Completion end frame
            assert responses[3].WhichOneof('type') == 'end'
            assert responses[3].end.state == ax_pb2.STATE_COMPLETED
            
        await server.stop(0)

    asyncio.run(_run())

def test_vertex_kwargs_from_env_returns_kwargs(monkeypatch):
    """GOOGLE_GENAI_USE_VERTEXAI + GOOGLE_CLOUD_{PROJECT,LOCATION} -> kwargs dict."""
    from python.antigravity.harness_server import _vertex_kwargs_from_env
    monkeypatch.setenv("GOOGLE_GENAI_USE_VERTEXAI", "True")
    monkeypatch.setenv("GOOGLE_CLOUD_PROJECT", "env-project")
    monkeypatch.setenv("GOOGLE_CLOUD_LOCATION", "us-east1")
    assert _vertex_kwargs_from_env() == {
        "vertex": True,
        "project": "env-project",
        "location": "us-east1",
    }


def test_vertex_kwargs_from_env_no_op_without_vertex(monkeypatch):
    """Without GOOGLE_GENAI_USE_VERTEXAI, returns empty dict."""
    from python.antigravity.harness_server import _vertex_kwargs_from_env
    monkeypatch.delenv("GOOGLE_GENAI_USE_VERTEXAI", raising=False)
    monkeypatch.delenv("GOOGLE_GENAI_USE_ENTERPRISE", raising=False)
    monkeypatch.setenv("GOOGLE_CLOUD_PROJECT", "should-be-ignored")
    monkeypatch.setenv("GOOGLE_CLOUD_LOCATION", "should-be-ignored")
    assert _vertex_kwargs_from_env() == {}


def test_vertex_kwargs_from_env_raises_when_project_missing(monkeypatch):
    """Vertex requested with no project -> ValueError naming the env var."""
    from python.antigravity.harness_server import _vertex_kwargs_from_env
    monkeypatch.setenv("GOOGLE_GENAI_USE_VERTEXAI", "True")
    monkeypatch.delenv("GOOGLE_CLOUD_PROJECT", raising=False)
    monkeypatch.setenv("GOOGLE_CLOUD_LOCATION", "us-east1")
    with pytest.raises(ValueError, match="GOOGLE_CLOUD_PROJECT"):
        _vertex_kwargs_from_env()


def test_vertex_kwargs_from_env_raises_when_location_missing(monkeypatch):
    """Vertex requested with no location -> ValueError naming the env var."""
    from python.antigravity.harness_server import _vertex_kwargs_from_env
    monkeypatch.setenv("GOOGLE_GENAI_USE_VERTEXAI", "True")
    monkeypatch.setenv("GOOGLE_CLOUD_PROJECT", "env-project")
    monkeypatch.delenv("GOOGLE_CLOUD_LOCATION", raising=False)
    with pytest.raises(ValueError, match="GOOGLE_CLOUD_LOCATION"):
        _vertex_kwargs_from_env()


def test_vertex_kwargs_from_env_enterprise_alias(monkeypatch):
    """GOOGLE_GENAI_USE_ENTERPRISE is an alias for VERTEXAI."""
    from python.antigravity.harness_server import _vertex_kwargs_from_env
    monkeypatch.delenv("GOOGLE_GENAI_USE_VERTEXAI", raising=False)
    monkeypatch.setenv("GOOGLE_GENAI_USE_ENTERPRISE", "true")
    monkeypatch.setenv("GOOGLE_CLOUD_PROJECT", "ent-project")
    monkeypatch.setenv("GOOGLE_CLOUD_LOCATION", "us-central1")
    assert _vertex_kwargs_from_env() == {
        "vertex": True,
        "project": "ent-project",
        "location": "us-central1",
    }


def test_build_default_config_picks_up_vertex_env(monkeypatch):
    """End-to-end: env vars flow through _build_default_config into LocalAgentConfig."""
    from python.antigravity.harness_server import _build_default_config
    monkeypatch.setenv("GOOGLE_GENAI_USE_VERTEXAI", "True")
    monkeypatch.setenv("GOOGLE_CLOUD_PROJECT", "env-project")
    monkeypatch.setenv("GOOGLE_CLOUD_LOCATION", "us-east1")
    cfg = _build_default_config()
    assert cfg.vertex is True
    assert cfg.project == "env-project"
    assert cfg.location == "us-east1"


def test_servicer_requires_default_config():
    """Constructor takes a default config; passing nothing is a TypeError."""
    with pytest.raises(TypeError, match="default_config"):
        AntigravityHarnessServiceServicer()


def test_run_turn_guards_against_missing_default_config(monkeypatch):
    """If something sets _default_config to None at runtime (future bug in
    per-request layering, #194), _run_turn returns STATE_FAILED instead of
    crashing the server.
    """
    async def _run():
        cfg = LocalAgentConfig(system_instructions="will be set to None")
        servicer = AntigravityHarnessServiceServicer(cfg)
        servicer._default_config = None
        server = grpc.aio.server()
        ax_pb2_grpc.add_HarnessServiceServicer_to_server(servicer, server)
        port = server.add_insecure_port("localhost:0")
        await server.start()
        try:
            async with grpc.aio.insecure_channel(f"localhost:{port}") as channel:
                stub = ax_pb2_grpc.HarnessServiceStub(channel)
                req = ax_pb2.HarnessRequest(
                    conversation_id="conv-guard",
                    harness_id="antigravity",
                    start=ax_pb2.HarnessStart(messages=[
                        ax_pb2.Message(role="user",
                            content=content_pb2.Content(text=content_pb2.TextContent(text="Hi"))),
                    ]),
                )
                async def request_iter():
                    yield req
                responses = []
                async for resp in stub.Connect(request_iter()):
                    responses.append(resp)
                assert len(responses) == 1
                assert responses[0].end.state == ax_pb2.STATE_FAILED
                assert responses[0].end.error.code == 9
                assert "Agent config is not loaded" in responses[0].end.error.description
        finally:
            await server.stop(0)
    asyncio.run(_run())

