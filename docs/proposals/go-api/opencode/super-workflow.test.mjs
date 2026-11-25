// Run with: node --test opencode/super-workflow.test.mjs
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { SuperWorkflowPlugin } from './super-workflow.js';

// fakeClient answers session.get from parents (id -> parentID); an id in
// failing throws once per entry left in failing[id].
function fakeClient(parents, failing = {}) {
  const calls = [];
  return {
    calls,
    session: {
      get: async ({ path: { id } }) => {
        calls.push(id);
        if (failing[id] > 0) { failing[id]--; throw new Error('lookup failed'); }
        if (!(id in parents)) return { data: undefined };
        return { data: { id, parentID: parents[id] } };
      },
    },
  };
}

async function envFor(hooks, input, inherited = {}) {
  const output = { env: { ...inherited } };
  await hooks['shell.env'](input, output);
  return output.env;
}

test('root and child sessions', async () => {
  const hooks = await SuperWorkflowPlugin({ client: fakeClient({ ses_r: undefined, ses_c: 'ses_r' }) });
  assert.deepEqual(await envFor(hooks, { sessionID: 'ses_r' }), { OPENCODE_SESSION_ID: 'ses_r', OPENCODE_PARENT_SESSION_ID: '' });
  assert.deepEqual(await envFor(hooks, { sessionID: 'ses_c' }), { OPENCODE_SESSION_ID: 'ses_c', OPENCODE_PARENT_SESSION_ID: 'ses_r' });
});

test('no session clears inherited variables', async () => {
  const hooks = await SuperWorkflowPlugin({ client: fakeClient({}) });
  const env = await envFor(hooks, { cwd: '/x' }, { OPENCODE_SESSION_ID: 'ses_outer', OPENCODE_PARENT_SESSION_ID: '' });
  assert.deepEqual(env, { OPENCODE_SESSION_ID: '', OPENCODE_PARENT_SESSION_ID: '' });
});

test('a failed lookup is unknown and is not cached', async () => {
  const client = fakeClient({ ses_c: 'ses_r' }, { ses_c: 1 });
  const hooks = await SuperWorkflowPlugin({ client });
  assert.equal((await envFor(hooks, { sessionID: 'ses_c' })).OPENCODE_PARENT_SESSION_ID, 'unknown');
  assert.equal((await envFor(hooks, { sessionID: 'ses_c' })).OPENCODE_PARENT_SESSION_ID, 'ses_r');
  assert.equal((await envFor(hooks, { sessionID: 'ses_c' })).OPENCODE_PARENT_SESSION_ID, 'ses_r');
  assert.deepEqual(client.calls, ['ses_c', 'ses_c'], 'a successful lookup is cached');
});

test('a session the server does not know is unknown', async () => {
  const hooks = await SuperWorkflowPlugin({ client: fakeClient({}) });
  assert.equal((await envFor(hooks, { sessionID: 'ses_x' })).OPENCODE_PARENT_SESSION_ID, 'unknown');
});
