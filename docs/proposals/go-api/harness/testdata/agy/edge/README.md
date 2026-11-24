# agy edge transcripts

Synthetic records for reader edge cases. Every transcript except
`user-last.jsonl` begins with a child delivery wrapper.

| File | Exercises |
| --- | --- |
| error-last.jsonl | A failed last step does not end the turn. |
| error-message.jsonl | A system error followed by a response does not prevent completion. |
| no-usage.jsonl | Missing token fields yield unknown usage. |
| read-file.jsonl | A file-read tool names the path it read. |
| tool-result.jsonl | A final tool result does not end the turn. |
| trailing-call.jsonl | A final tool call does not end the turn. Cache reads do not count toward usage. |
| truncated.jsonl | Readers skip a partial final JSON record. |
| user-last.jsonl | A user message alone provides neither an end marker nor tool evidence. |
| running-far-back.jsonl | A task still running before the 256 KiB tail window prevents completion. |

The far-back fixture starts one background task and follows it with 75
synthetic responses of roughly 4 KiB each, without a task completion message.
