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
from python.antigravity.harness_server import AntigravityHarnessServiceServicer, loaded_config
from google.antigravity import LocalAgentConfig

@pytest.fixture
def mock_config(monkeypatch):
    monkeypatch.setenv("GEMINI_API_KEY", "mock-api-key")
    cfg = LocalAgentConfig(system_instructions="Test instructions")
    import python.antigravity.harness_server as hs
    hs.loaded_config = cfg
    return cfg

def test_grpc_connect_success(mock_config, monkeypatch):
    async def _run():
        # 1. Start temporary local gRPC server on random open port
        server = grpc.aio.server()
        servicer = AntigravityHarnessServiceServicer()
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
        servicer = AntigravityHarnessServiceServicer()
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

        server = grpc.aio.server()
        ax_pb2_grpc.add_HarnessServiceServicer_to_server(AntigravityHarnessServiceServicer(), server)
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


def test_grpc_connect_missing_credentials(mock_config, monkeypatch):
    monkeypatch.delenv("GEMINI_API_KEY", raising=False)
    monkeypatch.delenv("GOOGLE_API_KEY", raising=False)
    monkeypatch.delenv("GOOGLE_GENAI_USE_VERTEXAI", raising=False)
    monkeypatch.delenv("GOOGLE_GENAI_USE_ENTERPRISE", raising=False)

    async def _run():
        server = grpc.aio.server()
        servicer = AntigravityHarnessServiceServicer()
        ax_pb2_grpc.add_HarnessServiceServicer_to_server(servicer, server)
        port = server.add_insecure_port("localhost:0")
        await server.start()
        
        addr = f"localhost:{port}"
        async with grpc.aio.insecure_channel(addr) as channel:
            stub = ax_pb2_grpc.HarnessServiceStub(channel)
            
            start_payload = ax_pb2.HarnessStart(
                messages=[
                    ax_pb2.Message(role="user", content=content_pb2.Content(text=content_pb2.TextContent(text="Hi")))
                ]
            )
            req = ax_pb2.HarnessRequest(
                conversation_id="conv-test-credentials",
                harness_id="antigravity",
                start=start_payload
            )
            
            async def request_iter():
                yield req

            responses = []
            async for resp in stub.Connect(request_iter()):
                responses.append(resp)
                
            assert len(responses) == 1
            assert responses[0].WhichOneof('type') == 'end'
            assert responses[0].end.state == ax_pb2.STATE_FAILED
            assert "No Gemini credentials configured" in responses[0].end.error_message
            assert "GEMINI_API_KEY" in responses[0].end.error_message
            
        await server.stop(0)

    asyncio.run(_run())


def test_grpc_connect_programmatic_credentials(monkeypatch):
    monkeypatch.delenv("GEMINI_API_KEY", raising=False)
    monkeypatch.delenv("GOOGLE_API_KEY", raising=False)
    monkeypatch.delenv("GOOGLE_GENAI_USE_VERTEXAI", raising=False)
    monkeypatch.delenv("GOOGLE_GENAI_USE_ENTERPRISE", raising=False)

    # Config with API key programmatically set
    cfg = LocalAgentConfig(system_instructions="Test instructions", api_key="mock-config-api-key")
    import python.antigravity.harness_server as hs
    hs.loaded_config = cfg

    async def _run():
        server = grpc.aio.server()
        servicer = AntigravityHarnessServiceServicer()
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


