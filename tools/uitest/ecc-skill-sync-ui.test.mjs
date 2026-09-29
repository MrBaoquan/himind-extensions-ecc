/**
 * ECC 分发面板的载荷契约测试。
 *
 * 宿主在拉起插件进程之前，先按 plugin.json 的 input_schema 校验入参；不合法就
 * 直接拒绝，插件代码根本收不到这次调用，界面上只剩一句「读取分发状态失败」。
 * 筛选器那个 bug 就是这么来的：没选时是空串，而 state 声明了 enum，空串不是
 * 任何一个枚举值，于是每次打开面板都失败。
 *
 * 这份测试不看 Go 侧逻辑，而是把 ui/app.js 真跑起来（假 DOM + 假宿主），把界面
 * 实际发出去的每一份载荷按宿主同一套规则验一遍：类型、枚举、上下限、未知字段。
 * 规则照抄 himind-agent src/capability/service.rs 的 validate_capability_value。
 *
 * 跑法：node tools/uitest/ecc-skill-sync-ui.test.mjs（go test ./tools/uitest 也会带上）
 */

import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import vm from 'node:vm';

const testsDir = dirname(fileURLToPath(import.meta.url));
const pluginDir = join(testsDir, '..', '..', 'plugins', 'ecc-skill-sync');
const manifest = JSON.parse(readFileSync(join(pluginDir, 'plugin.json'), 'utf8'));
const appSource = readFileSync(join(pluginDir, 'ui', 'app.js'), 'utf8');
const capabilities = new Map((manifest.capabilities || []).map((item) => [item.id, item]));

// ---------- 假宿主回话 ----------

const REPO = 'F:\\WebProjects\\himind-extensions-ecc';

const HINT = {
  current: { valid: false },
  candidates: [{ path: REPO, has_policy: true }],
};

const REPORT = {
  repository: 'affaan-m/ECC',
  upstream_commit: 'abcdef1234567890',
  commit_date: '2026-09-23T00:00:00Z',
  sequence: 7,
  totals: {
    skills: 3,
    distributed: 1,
    pending: 1,
    held: 1,
    excluded: 0,
    frozen: 0,
    published_versions: 1,
  },
  categories: [{ key: 'software-engineering', total: 3, excluded: 0 }],
  modules: [{ key: 'core', category: 'software-engineering', total: 3, excluded: 0 }],
  items: [{ slug: 'tdd', name: 'TDD', state: 'distributed' }],
  policy: { excluded_modules: {}, excluded_categories: {}, excluded_skills: {} },
  policy_path: join(REPO, 'dispatch-policy.json'),
};

const RESPONSES = {
  'ecc.sync.repo_hint': { repo_hint: HINT },
  'ecc.sync.distribution': REPORT,
  'ecc.sync.dispatch_policy_save': { saved: true },
  'ecc.sync.probe': { probe: { changed: false, upstream_commit: 'abcdef1234567890' } },
  'ecc.sync.publish': { publish: { published: 0, pending: 0, failed: 0 } },
};

// ---------- 假 DOM ----------

function fakeElement(id) {
  const listeners = new Map();
  return {
    id,
    value: '',
    checked: false,
    disabled: false,
    hidden: false,
    className: '',
    title: '',
    textContent: '',
    innerHTML: '',
    dataset: {},
    classList: { add() {}, remove() {}, toggle() {}, contains: () => false },
    addEventListener(type, handler) {
      if (!listeners.has(type)) listeners.set(type, []);
      listeners.get(type).push(handler);
    },
    dispatch(type, event = {}) {
      (listeners.get(type) || []).forEach((handler) => handler(event));
    },
    closest: () => null,
    querySelector: () => null,
    querySelectorAll: () => [],
    focus() {},
    blur() {},
    select() {},
    setAttribute() {},
    getAttribute: () => null,
    appendChild() {},
    removeChild() {},
  };
}

const settle = async (rounds = 12) => {
  for (let index = 0; index < rounds; index += 1) {
    await new Promise((resolve) => setTimeout(resolve, 0));
  }
};

// 把界面跑起来：所有能力调用都记进 calls，由假宿主按 capability 回话。
function bootUi() {
  const elements = new Map();
  const calls = [];
  const storage = new Map();
  const element = (id) => {
    if (!elements.has(id)) elements.set(id, fakeElement(id));
    return elements.get(id);
  };

  const window = {
    localStorage: {
      getItem: (key) => (storage.has(key) ? storage.get(key) : null),
      setItem: (key, value) => storage.set(key, String(value)),
      removeItem: (key) => storage.delete(key),
    },
    setTimeout,
    clearTimeout,
    __TAURI_INTERNALS__: {
      async invoke(command, payload) {
        calls.push({ command, payload });
        if (command === 'get_plugin_view_context') {
          return { workspace_root: 'F:\\WebProjects\\项目看板' };
        }
        if (command !== 'invoke_plugin_view_capability') return {};
        const id = payload && payload.capabilityId;
        if (!id) return {};
        return RESPONSES[id] || {};
      },
    },
  };

  const document = {
    getElementById: element,
    createElement: (tag) => fakeElement(tag),
    addEventListener() {},
    querySelector: () => null,
    body: fakeElement('body'),
  };

  vm.runInContext(
    appSource,
    vm.createContext({ window, document, setTimeout, clearTimeout, console }),
    { filename: 'ui/app.js' },
  );

  return { element, calls };
}

function inputsOf(calls, capabilityId) {
  return calls
    .filter(
      (call) =>
        call.command === 'invoke_plugin_view_capability' &&
        call.payload &&
        call.payload.capabilityId === capabilityId,
    )
    .map((call) => call.payload.input);
}

// ---------- 宿主那套契约校验 ----------

function matchesType(expected, value) {
  switch (expected) {
    case 'string':
      return typeof value === 'string';
    case 'object':
      return value !== null && typeof value === 'object' && !Array.isArray(value);
    case 'array':
      return Array.isArray(value);
    case 'integer':
      return Number.isInteger(value);
    case 'number':
      return typeof value === 'number';
    case 'boolean':
      return typeof value === 'boolean';
    case 'null':
      return value === null;
    default:
      return true;
  }
}

function validateValue(name, schema, value, problems) {
  if (!schema || typeof schema !== 'object') return;
  const expected = Array.isArray(schema.type)
    ? schema.type
    : schema.type
      ? [schema.type]
      : [];
  if (expected.length && !expected.some((kind) => matchesType(kind, value))) {
    problems.push(`${name} 类型不对：${JSON.stringify(value)}`);
  }
  if (Array.isArray(schema.enum) && !schema.enum.includes(value)) {
    problems.push(`${name} 不在枚举里：${JSON.stringify(value)}`);
  }
  if (typeof value === 'string') {
    if (schema.maxLength != null && [...value].length > schema.maxLength) {
      problems.push(`${name} 超长`);
    }
    if (schema.minLength != null && [...value].length < schema.minLength) {
      problems.push(`${name} 过短`);
    }
    if (schema.pattern) {
      // 宿主用的是有限子集正则，这里只挡明显的“空串配 pattern”。
      try {
        if (!new RegExp(`^(?:${schema.pattern})$`).test(value)) {
          problems.push(`${name} 不符合格式 ${schema.pattern}：${JSON.stringify(value)}`);
        }
      } catch (error) {
        /* 宿主不认的正则交给宿主判断 */
      }
    }
  }
  if (typeof value === 'number') {
    if (schema.minimum != null && value < schema.minimum) {
      problems.push(`${name} 小于下限 ${schema.minimum}`);
    }
    if (schema.maximum != null && value > schema.maximum) {
      problems.push(`${name} 超过上限 ${schema.maximum}`);
    }
  }
  if (Array.isArray(value)) {
    if (schema.items) {
      value.forEach((item, index) => validateValue(`${name}[${index}]`, schema.items, item, problems));
    }
    if (schema.minItems != null && value.length < schema.minItems) problems.push(`${name} 条目过少`);
    if (schema.maxItems != null && value.length > schema.maxItems) problems.push(`${name} 条目过多`);
  }
  if (value !== null && typeof value === 'object' && !Array.isArray(value)) {
    const properties = schema.properties || {};
    (schema.required || []).forEach((key) => {
      if (!(key in value)) problems.push(`${name} 缺少必填字段 ${key}`);
    });
    if (schema.additionalProperties === false) {
      Object.keys(value).forEach((key) => {
        if (!(key in properties)) problems.push(`${name} 出现未声明字段 ${key}`);
      });
    }
    Object.keys(value).forEach((key) => {
      if (key in properties) validateValue(`${name}.${key}`, properties[key], value[key], problems);
    });
  }
}

function validateCall(call, problems) {
  const id = call.payload.capabilityId;
  const capability = capabilities.get(id);
  if (!capability) {
    problems.push(`界面调了 Manifest 里没有的能力：${id}`);
    return;
  }
  const input = call.payload.input;
  if (input === null || typeof input !== 'object' || Array.isArray(input)) {
    problems.push(`${id} 的入参不是对象：${JSON.stringify(input)}`);
    return;
  }
  validateValue(id, capability.input_schema, input, problems);
}

// ---------- 场景 ----------

const problems = [];
const ui = bootUi();
await settle();

// 1. 开面板：自动定位仓库后读一次分发状态，筛选器没选就不该出现在载荷里。
const bootInputs = inputsOf(ui.calls, 'ecc.sync.distribution');
assert.ok(bootInputs.length > 0, '面板启动后应当自动读一次分发状态');
['state', 'category', 'module', 'keyword'].forEach((key) => {
  assert.ok(
    !(key in bootInputs[0]),
    `筛选器没选时不该把 ${key} 传下去（宿主会因为空串不在 enum 里直接拒绝）`,
  );
});

// 2. 选了状态：只有这一个筛选项下传。
ui.element('filter-state').value = 'held';
ui.element('filter-state').dispatch('change', {});
await settle();
const filtered = inputsOf(ui.calls, 'ecc.sync.distribution').at(-1);
assert.equal(filtered.state, 'held', '选中的状态应当下传');
assert.ok(!('category' in filtered) && !('module' in filtered), '没选的筛选项不该下传');

// 3. 关键字两侧空白要修掉；纯空白等于没填。
ui.element('filter-keyword').value = '  tdd  ';
ui.element('filter-keyword').dispatch('input', {});
await new Promise((resolve) => setTimeout(resolve, 400));
await settle();
assert.equal(
  inputsOf(ui.calls, 'ecc.sync.distribution').at(-1).keyword,
  'tdd',
  '关键字应当去掉两侧空白',
);

ui.element('filter-reset').dispatch('click', {});
await settle();
const reset = inputsOf(ui.calls, 'ecc.sync.distribution').at(-1);
['state', 'category', 'module', 'keyword'].forEach((key) => {
  assert.ok(!(key in reset), `重置筛选后不该传 ${key}`);
});

// 4. 其余动作照常走一遍，载荷一起进校验。
ui.element('probe').dispatch('click', {});
await settle();
ui.element('limit').value = '5';
ui.element('dry-run').dispatch('click', {});
await settle();
ui.element('publish').dispatch('click', {});
await settle();

// 5. 排除一条分类：要过原因弹窗，再写回整份策略。
ui.element('rail-body').dispatch('change', {
  target: {
    closest: () => ({
      checked: false,
      disabled: false,
      dataset: { kind: 'category', key: 'software-engineering' },
    }),
  },
});
await settle();
assert.equal(ui.element('modal').hidden, false, '排除分类应当先问原因');
ui.element('modal-form').dispatch('submit', { preventDefault() {} });
await settle();

const policyInputs = inputsOf(ui.calls, 'ecc.sync.dispatch_policy_save');
assert.ok(policyInputs.length > 0, '确认原因后应当写回分发策略');

// ---------- 结论 ----------

const capabilityCalls = ui.calls.filter((call) => call.command === 'invoke_plugin_view_capability');
capabilityCalls.forEach((call) => validateCall(call, problems));

// 顺带确认场景真的跑到了：没有调用被捕获时，上面的校验等于什么都没验。
['ecc.sync.distribution', 'ecc.sync.probe', 'ecc.sync.publish', 'ecc.sync.dispatch_policy_save'].forEach(
  (id) => {
    assert.ok(inputsOf(ui.calls, id).length > 0, `场景没跑到 ${id}，这份校验就是空转`);
  },
);

if (problems.length) {
  console.error('界面发出去的载荷过不了宿主契约校验：');
  problems.forEach((problem) => console.error('  - ' + problem));
  process.exit(1);
}

console.log(`面板载荷契约通过（${capabilityCalls.length} 次能力调用）`);
