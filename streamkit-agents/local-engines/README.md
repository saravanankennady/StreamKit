# Local engines

Real speech and language engines that run on a CPU with no keys, behind OpenAI-shaped HTTP APIs. They are how the framework is proved
end to end. **They are not part of the shipped package.**

| Engine | Model | Model license | Engine license | Size | Port in tests |
| --- | --- | --- | --- | --- | --- |
| [llama.cpp](https://github.com/ggml-org/llama.cpp) `llama-server`, build b12102 | [Qwen2.5 1.5B Instruct, q4_k_m](https://huggingface.co/Qwen/Qwen2.5-1.5B-Instruct-GGUF) | Apache-2.0, per the model page and its `LICENSE` | MIT | 1.1 GB | 57990 |
| [faster-whisper](https://github.com/SYSTRAN/faster-whisper), through `stt_server.py` | [Systran/faster-whisper-base.en](https://huggingface.co/Systran/faster-whisper-base.en) | MIT, per the model page | MIT | 141 MB | 57991 |
| [Piper](https://github.com/OHF-Voice/piper1-gpl), through `tts_server.py` | [en_US-ljspeech-medium](https://huggingface.co/rhasspy/piper-voices/tree/main/en/en_US/ljspeech/medium) | The dataset is public domain, per the model card, which gives no separate license for the voice | **GPL-3.0** | 61 MB | 57992 |
| Silero VAD, in the framework's process | [silero_vad.onnx](https://github.com/snakers4/silero-vad) | MIT | MIT | 2.3 MB | |

**Piper is GPL-3.0**, so it stays in a process of its own, reached over HTTP, and nothing in `streamkit_agents` imports it. Do not bundle
it with the product without deciding what that means for the product's license.

LiveKit's turn detector models are not here, and must not be: their license, `MODEL_LICENSE` in livekit/agents, forbids using them with any other framework.

## Setting up

Everything goes under `.toolchain/`, out of git.

```sh
uv venv --python 3.12 .toolchain/venvs/agents-engines
uv pip install --python .toolchain/venvs/agents-engines/bin/python faster-whisper piper-tts numpy
# The llama.cpp release tarball for ubuntu-x64 goes in .toolchain/llama.cpp, and the models in .toolchain/models.
```

## Running

```sh
.toolchain/venvs/agents-engines/bin/python stt_server.py --model .toolchain/models/whisper/base.en --port 57991
.toolchain/venvs/agents-engines/bin/python tts_server.py --voice .toolchain/models/piper/en_US-ljspeech-medium.onnx --port 57992
LD_LIBRARY_PATH=.toolchain/llama.cpp/llama-b12102 .toolchain/llama.cpp/llama-b12102/llama-server \
  -m .toolchain/models/llm/qwen2.5-1.5b-instruct-q4_k_m.gguf --host 127.0.0.1 --port 57990 -t 8 -c 4096 --jinja
```

Together they used about 2.3 GB of memory on the one machine this was run on. Models this small answer simple questions and can call a
tool, and they make plenty of mistakes. They are here to prove the pipeline, not to be good at conversation.
