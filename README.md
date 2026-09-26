<p align="center"><img src="assets/brand/github-banner.png" alt="fornax" width="100%"></p>

[![ci](https://github.com/earshot-run/fornax/actions/workflows/ci.yml/badge.svg)](https://github.com/earshot-run/fornax/actions/workflows/ci.yml)
[![license: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![latest release](https://img.shields.io/github/v/release/earshot-run/fornax?label=release)](https://github.com/earshot-run/fornax/releases/latest)

[English](README.md) · [简体中文](README.zh-CN.md) · [Command reference](docs/reference.md) · [Contributing](CONTRIBUTING.md)

fornax runs open models on your own computer. Chat with them, make images,
video and speech, have them make decisions, and keep everything on your
machine.

```sh
curl -fsSL https://raw.githubusercontent.com/earshot-run/fornax/main/install.sh | sh
fornax studio
```

![Chatting with a local model in fornax studio](assets/screens/studio-chat.gif)

## The studio

`fornax studio` opens in your browser. Pick a model, type, and it streams
back. Drop in a photo if the model can see, or talk to it if it can hear.
Your chats are saved.

![Making images in fornax studio](assets/screens/studio-image.png)

Image, video and voice work the same way: describe what you want, watch it
come in step by step, and find everything later in the gallery. Edit an
image by giving it a reference, or clone a voice from a few seconds of audio.

![Voice clips in fornax studio](assets/screens/studio-voice.png)

## Models

Open **Models** in the studio. It suggests a few good models for chat,
images, voice and video, tells you which ones fit your computer, and
downloads them with one click. You can also search Hugging Face for any
GGUF model and check its size before you download it. Gated models like
Llama need a Hugging Face token; add yours on the same page.

From a terminal it's one line, with any model on Hugging Face or Ollama:

```sh
fornax pull hf:Qwen/Qwen3-4B-GGUF
fornax ask hf:Qwen/Qwen3-4B-GGUF "Explain what a GGUF file is in one sentence."
```

If Hugging Face is slow or unavailable where you live, set `HF_ENDPOINT` to
an HTTPS mirror origin (for example `https://hf-mirror.com`). Model pins and
hash checks stay the same. Mirrors do not receive your Hugging Face token,
so use the official endpoint for gated models. See the
[download reference](docs/reference.md#downloads-and-hugging-face-tokens).

## Decisions

Some models don't chat. They answer typed questions about a piece of text
with calibrated probabilities: yes or no, one of a few options, or a level
on a scale. That's the job behind routing a support ticket, flagging a risky
message or grading an answer, and a local model does it in a fraction of a
second, for nothing.

```sh
fornax judge kev-4b \
  --state "Customer: I was charged twice this month and nobody has answered my last three emails. If this isn't fixed today, I'm cancelling." \
  --ask 'escalate|noul|Does this need a human today?' \
  --ask 'topic|choice|What is it about?|billing|bug|sales|other' \
  --ask 'mood|score|How upset is the customer?|calm|annoyed|angry'
```

```
  escalate noul    0.94  ███████████████████░
  topic    choice  billing
      billing 0.64  █████████████░░░░░░░
      other   0.29  ██████░░░░░░░░░░░░░░
      bug     0.06  █░░░░░░░░░░░░░░░░░░░
      sales   0.01  ░░░░░░░░░░░░░░░░░░░░
  mood     score   1.70 → angry
      angry   0.71  ██████████████░░░░░░
      annoyed 0.28  ██████░░░░░░░░░░░░░░
      calm    0.01  ░░░░░░░░░░░░░░░░░░░░
  179 ms
```

fornax ships two open families of these: [kev](https://github.com/jaredpalmer/kev),
Jev-style models from 0.8B to 27B, and Convai's smaller
[laya](https://github.com/NandhaKishorM/laya). Both speak TypeSafe's System
One API, so `fornax run kev-4b` gives you a local server the official
TypeSafe SDK talks to unchanged.

## Not enough GPU?

Run the studio on a bigger machine, a GPU box at home or a rented cloud GPU,
and use it from your laptop:

```sh
fornax studio -on me@gpu-box
```

fornax picks the right build for the hardware (Metal, CUDA, Vulkan or plain
CPU), and `fornax doctor` tells you which one it chose and why.

## Safe by default

Every download is pinned to an exact version and checked against its hash,
and model files are checked again before each run. Models only listen on
your own machine and need a key that fornax generates for you.

## For scripts and agents

Everything above also works from the terminal: `ask`, `see`, `hear`,
`imagine`, `say`, `embed`, `judge`, `run` and more, plus an MCP server for agents.
The full command reference and how it all works is in
[docs/reference.md](docs/reference.md).

## License

MIT. Model weights keep their own licenses.
