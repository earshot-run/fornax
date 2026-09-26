// fm-bridge — JSONL stdio bridge to Apple's FoundationModels framework.
// In:  {"id":N,"messages":[{"role":"…","content":"…"}],"stream":bool}
// Out: {"id":0,"ready":true} once, then {"id":N,"token":"…"}* {"id":N,"done":true}
//      or {"id":N,"error":"…"}. Exits when stdin closes.

import Foundation
import FoundationModels

@main
struct Bridge {
    static func send(_ obj: [String: Any]) {
        if let data = try? JSONSerialization.data(withJSONObject: obj),
           let line = String(data: data, encoding: .utf8) {
            FileHandle.standardOutput.write(Data(line.utf8 + [0x0a]))
        }
    }

    static func segments(_ text: String) -> [Transcript.Segment] {
        [.text(Transcript.TextSegment(content: text))]
    }

    static func main() async {
        let model = SystemLanguageModel.default
        switch model.availability {
        case .available:
            break
        case .unavailable(let reason):
            let why: String
            switch reason {
            case .deviceNotEligible: why = "this Mac is not eligible for Apple Intelligence"
            case .appleIntelligenceNotEnabled: why = "Apple Intelligence is off — enable it in System Settings"
            case .modelNotReady: why = "the on-device model is still downloading — try again shortly"
            default: why = "unavailable: \(reason)"
            }
            send(["id": 0, "error": why])
            return
        }
        send(["id": 0, "ready": true])

        while let line = readLine(strippingNewline: true) {
            guard let data = line.data(using: .utf8),
                  let req = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
                  let id = req["id"] as? Int,
                  let rawMessages = req["messages"] as? [[String: String]],
                  let last = rawMessages.last, last["role"] == "user" else {
                send(["id": -1, "error": "malformed request — needs id, messages, last message user"])
                continue
            }
            var entries: [Transcript.Entry] = []
            for m in rawMessages.dropLast() {
                switch m["role"] {
                case "system":
                    entries.append(.instructions(Transcript.Instructions(segments: segments(m["content"] ?? ""), toolDefinitions: [])))
                case "assistant":
                    entries.append(.response(Transcript.Response(assetIDs: [], segments: segments(m["content"] ?? ""))))
                default:
                    entries.append(.prompt(Transcript.Prompt(segments: segments(m["content"] ?? ""))))
                }
            }
            let session = LanguageModelSession(model: model, transcript: Transcript(entries: entries))
            let prompt = last["content"] ?? ""
            let stream = (req["stream"] as? Bool) ?? false
            do {
                if stream {
                    var previous = ""
                    for try await partial in session.streamResponse(to: prompt) {
                        let text = partial.content
                        let delta = text.hasPrefix(previous) ? String(text.dropFirst(previous.count)) : text
                        if !delta.isEmpty {
                            send(["id": id, "token": delta])
                        }
                        previous = text
                    }
                    send(["id": id, "done": true])
                } else {
                    let reply = try await session.respond(to: prompt)
                    send(["id": id, "reply": reply.content, "done": true])
                }
            } catch {
                send(["id": id, "error": String(describing: error)])
            }
        }
    }
}
