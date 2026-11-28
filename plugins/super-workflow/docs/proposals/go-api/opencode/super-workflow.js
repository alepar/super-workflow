// OpenCode plugin for the super-workflow engine.
//
// OpenCode 1.18 puts no session id in a shell command's environment, and a
// task subagent's commands inherit their parent's process environment. This
// plugin's shell.env hook exports the calling session and its parent session
// into every command the bash tool runs, so the engine can tell a child from
// its coordinator:
//
//   OPENCODE_SESSION_ID         the session running the command
//   OPENCODE_PARENT_SESSION_ID  its parent session: empty for a root session,
//                               "unknown" when the lookup failed
//
// OpenCode merges these over its own process environment, which may carry
// another OpenCode's values (an OpenCode started from a task shell), so both
// are always set, never left inherited: empty for a shell with no session.
//
// Install: copy or link this file into an OpenCode plugins directory
// (~/.config/opencode/plugins/ or <project>/.opencode/plugins/).

export const SuperWorkflowPlugin = async ({ client }) => {
  // A session's parent never changes: look each one up once. A failed lookup
  // is not cached, so a later command can try again.
  const parents = new Map();
  const parentOf = async (sessionID) => {
    if (parents.has(sessionID)) return parents.get(sessionID);
    try {
      const res = await client.session.get({ path: { id: sessionID } });
      if (!res?.data) return 'unknown';
      const parent = res.data.parentID || '';
      parents.set(sessionID, parent);
      return parent;
    } catch {
      return 'unknown';
    }
  };

  return {
    'shell.env': async (input, output) => {
      if (!input?.sessionID) {
        // A shell with no session (a terminal tab): clear what it inherited,
        // so it reads as no session rather than an outer one.
        output.env.OPENCODE_SESSION_ID = '';
        output.env.OPENCODE_PARENT_SESSION_ID = '';
        return;
      }
      output.env.OPENCODE_SESSION_ID = input.sessionID;
      output.env.OPENCODE_PARENT_SESSION_ID = await parentOf(input.sessionID);
    },
  };
};
