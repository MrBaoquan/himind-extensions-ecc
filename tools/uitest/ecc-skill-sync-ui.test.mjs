/**
 * ECC 技能库面板的载荷契约测试。
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
  // auto 是插件推定出来的仓库根目录；面板不再自己拼路径，也不再把它存进
  // localStorage——存一份会过期的副本，等于和仓库里的策略文件对着说话。
  auto: { path: REPO, count: 1, ambiguous: false },
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
  // 三条技能覆盖详情区的三种样子：有使用场景、缺场景句、已被排除。
  items: [
    {
      slug: 'tdd',
      skill_id: 'com.mrbaoquan.ecc.skill.tdd',
      name: 'TDD',
      description: '把需求拆成可验证的测试用例，再逐条落实。',
      scenario: '当你要给一个改动补上回归保护时用。',
      version: '1.0.0',
      module: 'core',
      categories: ['software-engineering'],
      source_path: 'skills/tdd',
      tier: 'A',
      state: 'distributed',
    },
    {
      slug: 'a11y-scan',
      name: '无障碍扫描',
      description: '扫出界面里对比度与命中区域不达标的元素。',
      version: '1.0.0',
      module: 'core',
      categories: ['software-engineering'],
      source_path: 'skills/a11y-scan',
      tier: 'B',
      state: 'held',
      detail: '市场文案还是机械推导的，校对后才发',
    },
    {
      slug: 'agent-diff',
      name: 'Agent 输出对比',
      description: '对两个 agent 的输出做逐项对比。',
      scenario: '只在需要判断哪一版更稳时用。',
      version: '1.0.0',
      module: 'core',
      categories: ['software-engineering'],
      source_path: 'skills/agent-diff',
      tier: 'A',
      state: 'excluded',
      rule: 'category',
      keyword: 'testing-quality',
      published_versions: 2,
    },
  ],
  policy: { excluded_modules: {}, excluded_categories: {}, excluded_skills: {} },
  policy_path: join(REPO, 'dispatch-policy.json'),
};

const RESPONSES = {
  'ecc.sync.repo-hint': { repo_hint: HINT },
  'ecc.sync.distribution': REPORT,
  'ecc.sync.dispatch-policy-save': { saved: true },
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
    // 界面用 setAttribute 同步 aria-expanded，这里记下来，断言能读到。
    attributes: {},
    dispatch(type, event = {}) {
      (listeners.get(type) || []).forEach((handler) => handler(event));
    },
    closest: () => null,
    querySelector: () => null,
    querySelectorAll: () => [],
    focus() {},
    blur() {},
    select() {},
    setAttribute(name, value) {
      this.attributes[name] = String(value);
    },
    removeAttribute(name) {
      delete this.attributes[name];
    },
    getAttribute(name) {
      return name in this.attributes ? this.attributes[name] : null;
    },
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
  // 场景需要自己控制某一类能力的回话时机（例如让先发的请求后到），默认全用上面的固定回话。
  const hooks = { distribution: null };
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
        if (id === 'ecc.sync.distribution' && hooks.distribution) {
          return hooks.distribution(payload.input || {});
        }
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

  // 假 DOM 的 hidden 默认是 false，而真实页面里这几块是带 hidden 属性渲染的。
  // 不还原这个初始状态，「错误提示」「详情区」「弹窗」的开关断言就是反的。
  ['panel-policy', 'read-error', 'policy-error', 'notice', 'modal', 'detail-body',
   'detail-scene-block', 'detail-note'].forEach((id) => {
    element(id).hidden = true;
  });

  return { element, calls, hooks };
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

// 1b. 首屏渲染是在「读取中」里完成的，详情区那个按钮不能因此留在禁用态——
// 否则一打开面板就点不动，用户以为坏了，换选一条才好。它不在 BUSY_DISABLED 名单里，
// 忙碌结束必须单独同步一次。
assert.equal(
  ui.element('detail-toggle').disabled,
  false,
  '首屏渲染完成后详情按钮应当可点',
);
assert.equal(
  ui.element('detail-toggle').dataset.slug,
  'tdd',
  '首屏要默认选中第一条技能，详情区才有内容',
);

// 2. 选了状态：计数条本身就是筛选入口，只有这一个筛选项下传。
assert.ok(
  ui.element('summary').innerHTML.includes('data-state="held"'),
  '计数条要给出四个状态的可点入口',
);
ui.element('summary').dispatch('click', {
  target: { closest: () => ({ dataset: { state: 'held' } }) },
});
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

// 4. 技能库要能翻：列表里带简介，点一条在详情里看它做什么、什么时候用。
const listHTML = ui.element('skill-list').innerHTML;
assert.ok(listHTML.includes('TDD'), '列表要显示技能名');
assert.ok(
  listHTML.includes('把需求拆成可验证的测试用例，再逐条落实。'),
  '列表要显示简介，用户才有理由点开某一条',
);
ui.element('skill-list').dispatch('click', {
  target: { closest: () => ({ dataset: { slug: 'tdd' } }) },
});
await settle();
assert.equal(
  ui.element('detail-intro').textContent,
  '把需求拆成可验证的测试用例，再逐条落实。',
  '详情要说明这条技能做什么',
);
assert.equal(
  ui.element('detail-scene').textContent,
  '当你要给一个改动补上回归保护时用。',
  '详情要说明什么时候用它',
);
assert.equal(ui.element('detail-scene-block').hidden, false, '有使用场景时不该藏起来');

// 缺场景句的那条：场景块收起，不拿编出来的话顶上。
ui.element('skill-list').dispatch('click', {
  target: { closest: () => ({ dataset: { slug: 'a11y-scan' } }) },
});
await settle();
assert.equal(ui.element('detail-name').textContent, '无障碍扫描', '点列表要换掉详情区的内容');
assert.equal(ui.element('detail-scene-block').hidden, true, '没有场景句时该收起场景块');
assert.equal(
  ui.element('detail-intro').textContent,
  '扫出界面里对比度与命中区域不达标的元素。',
  '没有场景句不影响简介',
);
assert.equal(
  ui.element('detail-note').textContent,
  '市场文案还是机械推导的，校对后才发',
  '卡住的原因要摆在状态下面',
);

// 已被排除的那条：动作换成「重新纳入分发」，不给一个重复的排除入口。
ui.element('skill-list').dispatch('click', {
  target: { closest: () => ({ dataset: { slug: 'agent-diff' } }) },
});
await settle();
assert.equal(
  ui.element('detail-toggle').textContent,
  '重新纳入分发',
  '已排除的技能要能重新纳入',
);

// 5. 重置筛选：四类条件一起清掉。
ui.element('filter-reset').dispatch('click', {});
await settle();
const reset = inputsOf(ui.calls, 'ecc.sync.distribution').at(-1);
['state', 'category', 'module', 'keyword'].forEach((key) => {
  assert.ok(!(key in reset), `重置筛选后不该传 ${key}`);
});

// 6. 读取入口只有顶栏那一个图标按钮：点它就该重新读一次。
assert.equal(ui.element('policy-count').textContent, '全部分发', '策略为空时要说明当前全部分发');
const beforeRefresh = inputsOf(ui.calls, 'ecc.sync.distribution').length;
ui.element('refresh').dispatch('click', {});
await settle();
assert.equal(
  inputsOf(ui.calls, 'ecc.sync.distribution').length,
  beforeRefresh + 1,
  '顶栏刷新按钮要重新读一次分发状态',
);

// 7. 检查上游更新在分发策略页，与手动补发放在一起。
ui.element('action-probe').dispatch('click', {});
await settle();
ui.element('limit').value = '5';
ui.element('dry-run').dispatch('click', {});
await settle();
ui.element('publish').dispatch('click', {});
await settle();

// 8. 排除一条分类：要过原因弹窗，再写回整份策略。
ui.element('policy-columns').dispatch('change', {
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

const policyInputs = inputsOf(ui.calls, 'ecc.sync.dispatch-policy-save');
assert.ok(policyInputs.length > 0, '确认原因后应当写回分发策略');

// 9. 纳入一整类：会重新推回市场，同样要先确认；取消确认不该落盘。
const beforeInclude = inputsOf(ui.calls, 'ecc.sync.dispatch-policy-save').length;
ui.element('policy-columns').dispatch('change', {
  target: {
    closest: () => ({
      checked: true,
      disabled: false,
      dataset: { kind: 'category', key: 'software-engineering' },
    }),
  },
});
await settle();
assert.equal(ui.element('modal').hidden, false, '纳入分类也要先确认');
assert.equal(ui.element('modal-field').hidden, true, '纳入不需要填原因，输入框要收起来');
ui.element('modal-cancel').dispatch('click', {});
await settle();
assert.equal(
  inputsOf(ui.calls, 'ecc.sync.dispatch-policy-save').length,
  beforeInclude,
  '取消确认后不该写回策略',
);

ui.element('policy-columns').dispatch('change', {
  target: {
    closest: () => ({
      checked: true,
      disabled: false,
      dataset: { kind: 'category', key: 'software-engineering' },
    }),
  },
});
await settle();
ui.element('modal-form').dispatch('submit', { preventDefault() {} });
await settle();
assert.equal(
  inputsOf(ui.calls, 'ecc.sync.dispatch-policy-save').length,
  beforeInclude + 1,
  '确认纳入后应当写回策略',
);

// 10. 连着改两次筛选、先发的那次后到：界面必须停在最新那次的结果上。
// 防抖只保证「敲字时不同时发」，发出去之后两次请求仍会重叠，这是搜索框最容易出错的地方。
ui.hooks.distribution = (input) => {
  const keyword = input.keyword || '';
  const items = REPORT.items.filter((item) => !keyword || item.slug === keyword);
  const delay = keyword === 'slow' ? 600 : 0;
  return new Promise((resolve) => {
    setTimeout(() => resolve(Object.assign({}, REPORT, { items })), delay);
  });
};
const keywordBox = ui.element('filter-keyword');
keywordBox.value = 'slow';
keywordBox.dispatch('input', {});
await new Promise((resolve) => setTimeout(resolve, 300)); // 防抖 260ms 后，slow 那次发出
keywordBox.value = 'tdd';
keywordBox.dispatch('input', {});
await new Promise((resolve) => setTimeout(resolve, 900)); // 等 slow 那次晚到的回话落地
await settle();
assert.ok(
  ui.element('skill-list').innerHTML.includes('TDD'),
  '晚到的旧结果不能盖掉新结果，否则搜索框是新条件、列表是旧结果',
);
assert.equal(
  ui.element('skill-list-foot').textContent,
  '筛选后 1 条 · 重置看全部',
  '脚注与列表必须出自同一份结果',
);

// ---------- 结论 ----------

const capabilityCalls = ui.calls.filter((call) => call.command === 'invoke_plugin_view_capability');
capabilityCalls.forEach((call) => validateCall(call, problems));

// 顺带确认场景真的跑到了：没有调用被捕获时，上面的校验等于什么都没验。
['ecc.sync.distribution', 'ecc.sync.probe', 'ecc.sync.publish', 'ecc.sync.dispatch-policy-save'].forEach(
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
