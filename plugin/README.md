# fornax for Earshot

Give every agent Earshot runs, not only the Earshot agent, the models fornax runs on your computer. Claude Code, Codex and the rest can look at an image, transcribe a recording, make an image, a video clip or speech, and embed text, without sending any of it to a cloud.

| Tool | What it does |
| --- | --- |
| `see` | Answers a question about an image, or describes it. |
| `hear` | Transcribes a recording, or answers a question about it. |
| `imagine` | Makes an image from a prompt, or from a still image to start from. |
| `animate` | Makes a short video clip from a prompt or a still. Takes minutes. |
| `say` | Speaks text to a WAV file, optionally in a voice cloned from a reference take. |
| `embed` | Turns text into an embedding vector. |
| `ask` | Sends one prompt to a local text or vision model. |
| `models` | Lists every model and whether it's installed. |

`imagine`, `animate` and `say` ask you before they run. Set any tool to Allow, Ask or Off in the plugin's **Tools** list.

## Add it

Install fornax first:

```sh
curl -fsSL https://raw.githubusercontent.com/earshot-run/fornax/main/install.sh | sh
```

Then paste this into **Settings ▸ Extensions ▸ Plugins ▸ Add from link** in Earshot:

```text
https://github.com/earshot-run/fornax/tree/main/plugin
```

Earshot runs `fornax mcp` and adds it as a connection, so every agent on that computer can call its tools. A tool uses the first installed model of its kind unless the call names one; `fornax pull` adds more.

## Where files go

Made files land in `~/.earshot/media` unless a call passes an absolute `out` path. That's `FORNAX_OUT_DIR`, which you can change on the plugin's page.
