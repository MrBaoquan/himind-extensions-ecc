/**
 * ECC 技能库面板。
 *
 * 这个窗口回答两件事，按使用频率排序：
 *   1. 上游这些技能分别做什么、什么时候该用 —— 「技能库」标签页，默认打开
 *   2. 哪些分类、模块或单条技能不参与分发 —— 「分发策略」标签页
 *
 * 界面只消费插件 Manifest 已声明的能力：读分发状态、写分发策略、探测上游、
 * 发布待发项。它不直接读写文件，也不拼 shell 命令，因此插件被单独打开或
 * 嵌进宿主时行为一致。
 *
 * 介绍文案的断句在 Go 侧（eccsync.SplitSkillCopy）：界面不认识「。」的拆法，
 * 同一段介绍就不会在两个地方被拆成两种样子。拆不出来时场景整块隐藏，
 * 不拿一句编的话顶上。
 *
 * 分发策略的判断同样全部落在 Go 侧：界面只回传「哪些分类/模块/单条技能被
 * 排除、各自的原因」，判定、优先级和落盘校验由 eccsync 负责。界面自算一套
 * 口径，迟早会出现「界面说会发、定时任务却跳过」这种最难查的不一致。
 */
(function () {
  'use strict';

  const DEFAULT_REASON = '按分发策略排除';
  const MAX_ITEMS = 500;

  const STATE_LABEL = {
    distributed: '已分发',
    pending: '待发布',
    held: '待校对文案',
    excluded: '已排除',
  };
  // 已分发是常态，用最淡的灰；待发布等着人动手，用主色；待校对是卡住的，用警告色。
  // 排除是主动策略、不是出错，所以也用中性灰：红色只留给读取与发布失败。
  const STATE_TONE = {
    distributed: '',
    pending: 'accent',
    held: 'warn',
    excluded: 'off',
  };
  // 计数条本身就是筛选入口：同一件事不再有「一排数字」和「一个下拉」两处说法。
  const SUMMARY_CHIPS = [
    { key: 'distributed', label: '已分发' },
    { key: 'pending', label: '待发布', tone: 'accent-on' },
    { key: 'held', label: '待校对文案', tone: 'warn-on' },
    { key: 'excluded', label: '已排除' },
  ];

  // 上游给的分类是英文键，界面上说人话。表与 Agent 的 categoryCatalog 同源，
  // 两边改要一起改，否则同一个分类在两个界面里叫两个名字。
  const CATEGORY_LABELS = {
    'software-engineering': '软件开发与工程',
    'visual-design': '平面与视觉',
    'video-post': '视频剪辑与后期',
    '3d-animation': '三维与动画',
    'content-production': '内容策划与制作',
    'audio-sound': '音频与声音',
    'data-automation': '数据与自动化',
    'docs-knowledge': '文档与知识',
    'testing-quality': '测试与质量',
    'collaboration-delivery': '协作与交付',
    'system-device': '系统与设备',
  };

  // 每个按钮只在它自己触发的动作里改写文案，其余按钮一律只置灰。
  // 串用会让「发布待发项」在纯读取时变成「读取中…」，看起来像在发布。
  // 刷新是个图标按钮，没有文字可改，用它转圈表示。
  const BASE_LABEL = {
    'dry-run': '预演',
    publish: '发布待发项',
    'action-probe': '检查上游更新',
    'repo-use': '读取此仓库',
  };
  const BUSY_ACTION = {
    probe: { id: 'action-probe', label: '探测中…' },
    'dry-run': { id: 'dry-run', label: '预演中…' },
    publish: { id: 'publish', label: '发布中…' },
  };
  const BUSY_DISABLED = ['dry-run', 'publish', 'action-probe', 'refresh'];

  const state = {
    tab: 'skills',
    repoRoot: '',
    report: null,
    selectedSlug: '',
    filters: { state: '', category: '', module: '', keyword: '' },
    busy: false,
    busyAction: '',
  };

  // 被排除的单条技能在策略页只剩一个 slug。名字从读到的报告里攒，攒过就一直用，
  // 这样筛掉某一类之后，策略页里的技能行不会退回成一串英文键。
  const skillNames = {};

  const el = (id) => document.getElementById(id);

  function bridgeInvoke(capabilityId, input) {
    const internals = window.__TAURI_INTERNALS__ || {};
    if (typeof internals.invoke !== 'function') {
      const error = new Error('当前不在 HiMind Agent 的扩展视图窗口中');
      error.code = 'bridge_unavailable';
      return Promise.reject(error);
    }
    return internals.invoke('invoke_plugin_view_capability', {
      capabilityId: capabilityId,
      input: input || {},
    });
  }

  function hostInvoke(command, payload) {
    const internals = window.__TAURI_INTERNALS__ || {};
    if (typeof internals.invoke !== 'function') return Promise.reject(new Error('宿主不可用'));
    return internals.invoke(command, payload || {});
  }

  function text(value) {
    return value === 0 || value ? String(value) : '';
  }

  function escapeHTML(value) {
    return text(value).replace(/[&<>"']/g, (char) => {
      return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[char];
    });
  }

  function int(value) {
    const parsed = Number(value);
    return Number.isFinite(parsed) ? parsed : 0;
  }

  function categoryLabel(key) {
    const name = text(key);
    return CATEGORY_LABELS[name] || name || '未归类';
  }

  function notify(message, tone) {
    const node = el('notice');
    if (!message) {
      node.hidden = true;
      node.textContent = '';
      node.className = 'notice';
      return;
    }
    node.hidden = false;
    node.className = 'notice' + (tone ? ' ' + tone : '');
    node.textContent = message;
  }

  // 就近报错：整份结果没读出来时，提示要贴在列表上方，而不是只在顶栏飘一条全局消息。
  function readError(message) {
    const node = el('read-error');
    if (!message) {
      node.hidden = true;
      node.textContent = '';
      return;
    }
    node.hidden = false;
    node.textContent = message;
  }

  // 切换开关失败时，提示贴在策略列旁边：视线就在那儿，不该跑到顶栏去找。
  function policyError(message) {
    const node = el('policy-error');
    if (!message) {
      node.hidden = true;
      node.textContent = '';
      return;
    }
    node.hidden = false;
    node.textContent = message;
  }

  function setBusy(busy, action) {
    state.busy = !!busy;
    state.busyAction = busy ? action || '' : '';
    BUSY_DISABLED.forEach((id) => {
      const node = el(id);
      if (node) node.disabled = state.busy;
    });
    syncDetailToggle();
    const refresh = el('refresh');
    if (refresh && refresh.classList) refresh.classList.toggle('is-busy', state.busy);
    const pending = state.busy ? BUSY_ACTION[action] : null;
    Object.keys(BASE_LABEL).forEach((id) => {
      const node = el(id);
      if (!node) return;
      node.textContent = pending && pending.id === id ? pending.label : BASE_LABEL[id];
    });
    syncRepoUse();
  }

  // 详情区那个按钮的可用态只看两件事：是否在忙、有没有选中技能。它不在
  // BUSY_DISABLED 名单里，所以忙碌结束时必须在这里单独恢复一次——首次加载是
  // 先 setBusy(true) 再 render()，按钮在忙碌中被画成禁用，若不回头同步就会一直
  // 灰着，看起来像点不动。
  function syncDetailToggle() {
    const toggle = el('detail-toggle');
    if (!toggle) return;
    toggle.disabled = state.busy || !toggle.dataset.slug;
  }

  // 「读取此仓库」只在填了新的、和当前不同的路径时可点。
  function syncRepoUse() {
    const node = el('repo-use');
    const typed = el('repo').value.trim();
    node.disabled = state.busy || !typed || samePath(typed, state.repoRoot);
  }

  function reason(error) {
    if (!error) return '未知错误';
    if (typeof error === 'string') return error;
    return error.message || JSON.stringify(error);
  }

  // ---------- 标签页 ----------

  function showTab(name) {
    state.tab = name === 'policy' ? 'policy' : 'skills';
    const skills = state.tab === 'skills';
    el('tab-skills').setAttribute('aria-selected', skills ? 'true' : 'false');
    el('tab-policy').setAttribute('aria-selected', skills ? 'false' : 'true');
    el('panel-skills').hidden = !skills;
    el('panel-policy').hidden = skills;
  }

  function bindTabs() {
    const tabs = [el('tab-skills'), el('tab-policy')];
    const names = ['skills', 'policy'];
    tabs.forEach((tab, index) => {
      tab.addEventListener('click', () => showTab(names[index]));
      // 左右箭头在标签间来回：这是标签页该有的键盘行为，不该只靠鼠标。
      tab.addEventListener('keydown', (event) => {
        const step = event.key === 'ArrowRight' ? 1 : event.key === 'ArrowLeft' ? -1 : 0;
        if (!step) return;
        event.preventDefault();
        const next = (index + step + tabs.length) % tabs.length;
        showTab(names[next]);
        tabs[next].focus();
      });
    });
  }

  // ---------- 仓库根目录 ----------

  function adoptRepoRoot(path) {
    state.repoRoot = path;
    el('repo').value = path;
    el('policy-path').textContent = path;
    el('policy-path').title = path;
    syncRepoUse();
  }

  async function workspaceFromHost() {
    try {
      const context = await hostInvoke('get_plugin_view_context');
      return (context && context.workspace_root) || '';
    } catch (error) {
      return '';
    }
  }

  // 仓库根目录的推断交给插件：只有它读得到「本插件是从哪个本地市场源装的」，
  // 而宿主给的 workspace_root 说的是这次开发任务开在哪个目录，两者常不是一回事。
  //
  // 返回 null 表示这条能力不可用（例如插件被单独打开），调用方据此退回旧行为。
  async function repoHint(path) {
    try {
      const response = await bridgeInvoke('ecc.sync.repo-hint', {
        repo_root: path || '',
        timeout_seconds: 60,
      });
      return (response && response.repo_hint) || null;
    } catch (error) {
      return null;
    }
  }

  function samePath(left, right) {
    const normalize = (value) => text(value).replace(/[\\/]+$/, '').toLowerCase();
    const normalized = normalize(left);
    return normalized !== '' && normalized === normalize(right);
  }

  function usableCandidates(hint) {
    return ((hint && hint.candidates) || []).filter((item) => item && item.has_policy && item.path);
  }

  function isRepoPathError(error) {
    const message = reason(error);
    return message.indexOf('不是 ECC 同步仓库') >= 0 || message.indexOf('需要 repo_root') >= 0;
  }

  // 手里的路径已经不能用了：换一个候选，换不动就照实报错。
  // 仓库路径每次打开现推，不再往 localStorage 里存一份会过期的副本——
  // 那份副本和仓库里的 extension-sources.json 是两个真相，谁对谁错没法判。
  async function recoverRepoRoot() {
    const hint = await repoHint(state.repoRoot);
    const next = usableCandidates(hint).find((item) => !samePath(item.path, state.repoRoot));
    if (!next) return '';
    adoptRepoRoot(next.path);
    return next.path;
  }

  async function boot() {
    const hint = await repoHint('');
    const auto = (hint && hint.auto) || null;
    if (auto && auto.path) {
      adoptRepoRoot(auto.path);
      const others = int(auto.count) - 1;
      const note = others > 0
        ? '已自动定位 ECC 仓库：' + auto.path + '（本机另有 ' + others + ' 个候选，可在上方改写）'
        : '';
      await reload(note);
      return;
    }
    // 推断能力不可用的老宿主：退回工作区路径，至少让人有个能改的起点。
    if (!hint) {
      const workspace = await workspaceFromHost();
      if (workspace) {
        adoptRepoRoot(workspace);
        await reload('没能推断出 ECC 仓库，先按当前工作区读了一次，不对就改上面那行路径。');
        return;
      }
    }
    notify('');
    readError('没找到 ECC 仓库，请在上面填写 himind-extensions-ecc 仓库根目录。');
    waitForRepo();
    el('repo').focus();
  }

  // 还没定位到仓库时，三块内容都不该停在「正在读取…」，那会一直读到天荒地老。
  function waitForRepo() {
    state.report = null;
    state.selectedSlug = '';
    el('summary').innerHTML = '';
    el('brand-sub').textContent = '上游技能库';
    el('policy-count').textContent = '—';
    el('category-list').innerHTML = '<li class="rail-empty">等待仓库路径</li>';
    el('module-list').innerHTML = '<li class="rail-empty">等待仓库路径</li>';
    el('skill-rule-list').innerHTML = '';
    el('skill-rule-hint').hidden = false;
    el('skill-list').innerHTML = '<li><p class="empty">先指定 ECC 仓库根目录，再看技能库。</p></li>';
    el('skill-list-foot').textContent = '';
    el('detail-empty').hidden = false;
    el('detail-body').hidden = true;
  }

  async function useTypedRepoRoot() {
    const path = el('repo').value.trim();
    if (!path) {
      readError('先填上 himind-extensions-ecc 仓库根目录。');
      return;
    }
    adoptRepoRoot(path);
    await reload();
  }

  // ---------- 读取与渲染 ----------

  // 宿主会在拉起插件进程前，先按 plugin.json 的契约校验入参：声明了 enum 的
  // 字段传空串会被直接拒掉（插件代码根本收不到这次调用）。筛选器「不限」时
  // 值是空串，所以这里统一只在真有值时带上，别把空筛选当参数送出去。
  function filterPayload() {
    const payload = {};
    ['state', 'category', 'module', 'keyword'].forEach((key) => {
      const value = state.filters[key];
      if (value) payload[key] = value;
    });
    return payload;
  }

  // 一次读取要几百毫秒，而筛选是防抖触发的：连着改两次条件，两份请求会同时在路上，
  // 先发的可能后到。只认最后一次发出的那份，早到的结果直接丢掉——否则界面会停在
  // 「搜索框是新条件、列表是旧结果」的样子，而且没人能一眼看出它是怎么来的。
  let loadSeq = 0;

  async function reload(note) {
    if (!state.repoRoot) {
      notify('先填上 himind-extensions-ecc 仓库根目录，再读技能库。', 'warn');
      return;
    }
    const seq = (loadSeq += 1);
    notify('');
    readError('');
    setBusy(true, 'reload');
    // 还没有结果时给个占位；已经有结果时保留旧列表，只在脚注上写「读取中」。
    // 否则每敲一下搜索框就整列清空再重填，闪得没法用。
    if (!state.report) {
      el('skill-list').innerHTML = '<li><p class="skeleton">正在读取技能库…</p></li>';
    }
    el('skill-list-foot').textContent = '读取中…';
    try {
      const report = await bridgeInvoke(
        'ecc.sync.distribution',
        Object.assign({
          repo_root: state.repoRoot,
          limit: MAX_ITEMS,
          timeout_seconds: 120,
        }, filterPayload()),
      );
      if (seq !== loadSeq) return;
      state.report = report;
      (report.items || []).forEach((item) => {
        if (item.slug && item.name) skillNames[item.slug] = item.name;
      });
      render();
      notify(note || '', note ? 'ok' : '');
    } catch (error) {
      if (seq !== loadSeq) return;
      if (isRepoPathError(error)) {
        const recovered = await recoverRepoRoot();
        if (recovered) return reload('已自动改用 ' + recovered);
      }
      state.report = null;
      renderEmptyRead(error);
    } finally {
      // 这次读取已经被更新的请求取代：忙碌状态归最新那次管，这里不提前放开按钮。
      if (seq === loadSeq) setBusy(false);
    }
  }

  // 读取失败后清掉旧渲染，别让上一次的结果继续挂在页面上。
  function renderEmptyRead(error) {
    state.selectedSlug = '';
    el('summary').innerHTML = '';
    el('skill-list').innerHTML = '<li><p class="empty">读取分发状态失败。</p></li>';
    el('skill-list-foot').textContent = '';
    el('detail-empty').hidden = false;
    el('detail-body').hidden = true;
    el('policy-count').textContent = '—';
    el('category-list').innerHTML = '<li class="rail-empty">读取失败，未加载</li>';
    el('module-list').innerHTML = '<li class="rail-empty">读取失败，未加载</li>';
    el('skill-rule-list').innerHTML = '';
    readError('读取分发状态失败：' + reason(error));
    notify('');
  }

  function render() {
    const report = state.report;
    if (!report) return;
    readError('');
    renderBrandSub(report);
    renderSummary(report);
    renderFilters(report);
    renderPolicy(report);
    reconcileSelection(report.items || []);
    renderSkillList(report);
    renderDetail(report);
  }

  function renderBrandSub(report) {
    const commit = text(report.upstream_commit).slice(0, 8);
    const parts = [];
    if (report.repository) parts.push(report.repository);
    if (commit) parts.push('上游 ' + commit);
    if (report.commit_date) parts.push(text(report.commit_date).slice(0, 10));
    if (report.sequence) parts.push('第 ' + report.sequence + ' 次同步');
    el('brand-sub').textContent = parts.join(' · ') || '上游技能库';
  }

  // 计数条：一条总数加四个状态。数值本身不上色，0 条待发布涂成蓝色只会让人以为那一格在报警。
  function renderSummary(report) {
    const totals = report.totals || {};
    const parts = [
      '<span class="summary-total">' + int(totals.skills) + ' 条技能</span>',
    ];
    const counts = {
      distributed: int(totals.distributed),
      pending: int(totals.pending),
      held: int(totals.held),
      excluded: int(totals.excluded),
    };
    SUMMARY_CHIPS.forEach((chip) => {
      const value = counts[chip.key];
      const on = state.filters.state === chip.key;
      const tone = on || value > 0 ? chip.tone || '' : '';
      let title = (on ? '再点一次取消，' : '只看') + chip.label + '的 ' + value + ' 条';
      if (chip.key === 'excluded' && int(totals.frozen)) {
        title += '；其中 ' + int(totals.frozen) + ' 条已经发过版本，市场里还留着';
      }
      parts.push(
        '<button type="button" class="state-chip ' + tone + '" data-state="' + chip.key + '"' +
        ' aria-pressed="' + (on ? 'true' : 'false') + '" title="' + escapeHTML(title) + '">' +
        escapeHTML(chip.label) + ' <b>' + value + '</b></button>',
      );
    });
    el('summary').innerHTML = parts.join('');
  }

  function renderFilters(report) {
    fillSelect(
      el('filter-category'),
      (report.categories || []).map((group) => ({ value: group.key, label: categoryLabel(group.key) })),
      '全部分类',
    );
    fillSelect(
      el('filter-module'),
      (report.modules || []).map((group) => ({ value: group.key, label: group.key })),
      '全部模块',
    );
    el('filter-category').value = state.filters.category;
    el('filter-module').value = state.filters.module;
    // 选项里已经没有的筛选值（换了仓库）当场清掉，免得下一份请求带着一个不存在的分类。
    state.filters.category = el('filter-category').value;
    state.filters.module = el('filter-module').value;
  }

  function fillSelect(select, entries, placeholder) {
    const current = select.value;
    const options = ['<option value="">' + escapeHTML(placeholder) + '</option>']
      .concat(entries.map((entry) => {
        return '<option value="' + escapeHTML(entry.value) + '">' + escapeHTML(entry.label) + '</option>';
      }))
      .join('');
    select.innerHTML = options;
    if (entries.some((entry) => entry.value === current)) select.value = current;
  }

  // ---------- 技能库 ----------

  // 选中项跟着结果走：筛完之后原来那条不在了，就落到第一条，别留一个空白的详情区。
  function reconcileSelection(items) {
    if (!items.length) {
      state.selectedSlug = '';
      return;
    }
    const kept = items.some((item) => text(item.slug) === state.selectedSlug);
    if (!kept) state.selectedSlug = text(items[0].slug);
  }

  function renderSkillList(report) {
    const items = report.items || [];
    const filtered = ['state', 'category', 'module', 'keyword'].some((key) => state.filters[key]);
    if (!items.length) {
      el('skill-list').innerHTML = '<li><p class="empty">' + (filtered
        ? '没有符合当前条件的技能，点「重置」看全部。'
        : '这个仓库里还没有可分发的技能，先在 HiMind 里跑一次「ECC 技能同步」工作流。') + '</p></li>';
    } else {
      el('skill-list').innerHTML = items.map(skillCard).join('');
    }
    const parts = [(filtered ? '筛选后 ' : '共 ') + items.length + ' 条'];
    const truncated = int(report.truncated);
    if (truncated) parts.push('另有 ' + truncated + ' 条未展开');
    if (filtered) parts.push('重置看全部');
    el('skill-list-foot').textContent = parts.join(' · ');
  }

  function skillCard(item) {
    const slug = text(item.slug);
    const categories = (item.categories || [])
      .map((key) => '<span class="chip">' + escapeHTML(categoryLabel(key)) + '</span>')
      .join('');
    const moduleChip = item.module
      ? '<span class="chip">' + escapeHTML(item.module) + '</span>'
      : '';
    return (
      '<li><button type="button" class="skill-card" data-slug="' + escapeHTML(slug) + '"' +
      (slug === state.selectedSlug ? ' aria-current="true"' : '') + '>' +
      '<span class="skill-card-top">' +
      '<span class="skill-card-name">' + escapeHTML(item.name || slug) + '</span>' +
      stateBadge(text(item.state)) +
      '</span>' +
      '<span class="skill-card-intro">' +
      escapeHTML(text(item.description) || '这条技能还没有填写介绍。') +
      '</span>' +
      '<span class="skill-card-meta">' + categories + moduleChip + '</span>' +
      '</button></li>'
    );
  }

  function stateBadge(stateKey) {
    return '<span class="badge ' + (STATE_TONE[stateKey] || '') + '">' +
      escapeHTML(STATE_LABEL[stateKey] || stateKey) + '</span>';
  }

  function renderDetail(report) {
    const items = (report && report.items) || [];
    const item = items.find((entry) => text(entry.slug) === state.selectedSlug) || null;
    el('detail-empty').hidden = !!item;
    el('detail-body').hidden = !item;
    if (!item) {
      // 没有选中项时清掉残留的技能 ID，免得之后同步按钮状态时把它当成「已选中」。
      const stale = el('detail-toggle');
      delete stale.dataset.slug;
      delete stale.dataset.action;
      return;
    }

    const stateKey = text(item.state);
    el('detail-name').textContent = text(item.name) || text(item.slug);
    el('detail-path').textContent = text(item.source_path) || text(item.skill_id) || text(item.slug);
    el('detail-badges').innerHTML = detailBadges(item, stateKey);
    el('detail-intro').textContent = text(item.description) || '这条技能还没有填写介绍。';

    const scenario = text(item.scenario);
    el('detail-scene-block').hidden = !scenario;
    el('detail-scene').textContent = scenario;

    const facts = [
      ['功能分类', (item.categories || []).map(categoryLabel).join('、') || '未归类'],
      ['上游模块', text(item.module) || '—'],
      ['技能版本', text(item.version) || '—'],
      ['分发状态', STATE_LABEL[stateKey] || stateKey],
    ];
    if (item.rule) facts.push(['命中规则', ruleText(item.rule, item.keyword)]);
    if (int(item.published_versions) > 0) {
      facts.push(['历史版本', '已发出 ' + int(item.published_versions) + ' 个']);
    }
    el('detail-facts').innerHTML = facts.map((pair) => {
      return '<dt>' + escapeHTML(pair[0]) + '</dt><dd>' + escapeHTML(pair[1]) + '</dd>';
    }).join('');

    // 排除原因、待校对卡在哪：这两句是「它为什么现在这样」的答案，就摆在状态下面。
    const note = text(item.detail);
    el('detail-note').hidden = !note;
    el('detail-note').textContent = note;

    const excluding = stateKey === 'excluded';
    const toggle = el('detail-toggle');
    toggle.textContent = excluding ? '重新纳入分发' : '从分发中排除';
    toggle.dataset.slug = text(item.slug);
    toggle.dataset.action = excluding ? 'include' : 'exclude';
    syncDetailToggle();
  }

  function detailBadges(item, stateKey) {
    const parts = [stateBadge(stateKey)];
    if (item.tier) parts.push('<span class="badge">' + escapeHTML(item.tier) + ' 档</span>');
    if (item.version) parts.push('<span class="badge">v' + escapeHTML(item.version) + '</span>');
    (item.categories || []).forEach((key) => {
      parts.push('<span class="badge">' + escapeHTML(categoryLabel(key)) + '</span>');
    });
    if (item.module) parts.push('<span class="badge">' + escapeHTML(item.module) + '</span>');
    return parts.join('');
  }

  function selectSkill(slug) {
    if (!slug || slug === state.selectedSlug) return;
    state.selectedSlug = slug;
    syncListSelection();
    renderDetail(state.report || {});
  }

  // 换选中项只改 aria-current：整列 268 张卡片重画一遍，点一下就要等一次全量布局，
  // 而翻目录是一路点下去的。列表内容没变，就没有理由重建 DOM。
  function syncListSelection() {
    const cards = el('skill-list').querySelectorAll('button[data-slug]');
    Array.prototype.forEach.call(cards, (card) => {
      if (card.dataset.slug === state.selectedSlug) card.setAttribute('aria-current', 'true');
      else card.removeAttribute('aria-current');
    });
  }

  function bindSkillList() {
    el('skill-list').addEventListener('click', (event) => {
      const button = event.target.closest ? event.target.closest('button[data-slug]') : null;
      if (!button) return;
      selectSkill(button.dataset.slug);
    });
    // 上下键在列表里走：一屏能看十来条，用键盘浏览时不该每换一条都回一次鼠标。
    el('skill-list').addEventListener('keydown', (event) => {
      const step = event.key === 'ArrowDown' ? 1 : event.key === 'ArrowUp' ? -1 : 0;
      if (!step) return;
      const buttons = Array.prototype.slice.call(el('skill-list').querySelectorAll('button[data-slug]'));
      if (buttons.length < 2) return;
      const index = buttons.indexOf(event.target);
      const next = buttons[Math.min(buttons.length - 1, Math.max(0, (index < 0 ? 0 : index) + step))];
      if (!next) return;
      event.preventDefault();
      selectSkill(next.dataset.slug);
      window.setTimeout(() => {
        const node = el('skill-list').querySelector('button[data-slug="' + next.dataset.slug + '"]');
        if (node && node.focus) node.focus();
      }, 0);
    });
  }

  function bindDetail() {
    el('detail-toggle').addEventListener('click', async () => {
      const button = el('detail-toggle');
      if (state.busy || !button.dataset.slug) return;
      button.disabled = true;
      const saved = await toggleRule('skill', button.dataset.slug, button.dataset.action === 'exclude');
      // 没落盘就按服务端最后一份策略重画，别把界面留在「看起来已经改了」的状态。
      if (!saved) {
        if (state.report) renderDetail(state.report);
        else button.disabled = false;
      }
    });
  }

  // ---------- 分发策略 ----------

  function renderPolicy(report) {
    const policy = report.policy || {};
    const excludedModules = policy.excluded_modules || {};
    const excludedCategories = policy.excluded_categories || {};
    const excludedSkills = policy.excluded_skills || {};
    const ruleCount =
      Object.keys(excludedModules).length +
      Object.keys(excludedCategories).length +
      Object.keys(excludedSkills).length;
    el('policy-count').textContent = ruleCount ? ruleCount + ' 条规则' : '全部分发';
    el('policy-path').textContent = report.policy_path || state.repoRoot;
    el('policy-path').title = report.policy_path || state.repoRoot;

    el('category-list').innerHTML = (report.categories || [])
      .map((group) => {
        const direct = excludedCategories[group.key] !== undefined;
        return ruleItem({
          kind: 'category',
          key: group.key,
          total: int(group.total),
          excluded: int(group.excluded),
          checked: !direct,
          reason: direct ? excludedCategories[group.key] : '',
          tone: direct ? 'off' : '',
          label: categoryLabel(group.key),
        });
      })
      .join('') || '<li class="rail-empty">上游没有声明功能分类</li>';

    el('module-list').innerHTML = (report.modules || [])
      .map((group) => {
        const direct = excludedModules[group.key] !== undefined;
        const parent = group.category ? excludedCategories[group.category] : '';
        const inherited = !direct && parent !== '' && parent !== undefined;
        return ruleItem({
          kind: 'module',
          key: group.key,
          total: int(group.total),
          excluded: int(group.excluded),
          checked: !direct && !inherited,
          disabled: inherited,
          reason: direct
            ? excludedModules[group.key]
            : inherited
              ? '随分类 ' + categoryLabel(group.category) + ' 排除'
              : '',
          tone: direct ? 'off' : inherited ? 'inherited' : '',
        });
      })
      .join('') || '<li class="rail-empty">上游没有声明模块</li>';

    const skillKeys = Object.keys(excludedSkills).sort();
    el('skill-rule-list').innerHTML = skillKeys
      .map((slug) => ruleItem({
        kind: 'skill',
        key: slug,
        total: 1,
        excluded: 1,
        checked: false,
        reason: excludedSkills[slug],
        tone: 'off',
        label: skillNames[slug] || slug,
      }))
      .join('');
    el('skill-rule-hint').hidden = skillKeys.length > 0;
  }

  function ruleItem(options) {
    const classes = ['rule-item', options.tone].filter(Boolean).join(' ');
    const metaParts = [];
    if (options.kind === 'skill') metaParts.push('单条技能');
    else metaParts.push(int(options.total) + ' 条');
    if (options.excluded) metaParts.push('排除 ' + int(options.excluded) + ' 条');
    const reason = options.reason
      ? '<span class="rule-reason" title="' + escapeHTML(options.reason) + '">' +
        escapeHTML(options.reason) + '</span>'
      : '';
    // 关掉的项只有删除线太轻，加一个「已排除」让它在一列里能被一眼扫到。
    const flag = options.tone === 'off' ? '<span class="rule-flag">已排除</span>' : '';
    return (
      '<li class="' + classes + '">' +
      '<label class="switch"><input type="checkbox" data-kind="' + escapeHTML(options.kind) + '"' +
      ' data-key="' + escapeHTML(options.key) + '"' +
      (options.checked ? ' checked' : '') +
      (options.disabled ? ' disabled' : '') + ' /><span class="knob"></span></label>' +
      '<span class="rule-text">' +
      '<span class="rule-name" title="' + escapeHTML(options.key) + '">' +
      escapeHTML(options.label || options.key) + '</span>' +
      '<span class="rule-meta">' + escapeHTML(metaParts.join(' · ')) + '</span>' +
      reason +
      flag +
      '</span></li>'
    );
  }

  function ruleText(rule, keyword) {
    const names = { skill: '单条技能', module: '上游模块', category: '功能分类' };
    const label = names[rule] || rule;
    if (!keyword) return label;
    return label + '「' + (rule === 'category' ? categoryLabel(keyword) : keyword) + '」';
  }

  // ---------- 写入策略 ----------

  function policyDraft() {
    const policy = (state.report && state.report.policy) || {};
    return {
      excluded_modules: Object.assign({}, policy.excluded_modules || {}),
      excluded_categories: Object.assign({}, policy.excluded_categories || {}),
      excluded_skills: Object.assign({}, policy.excluded_skills || {}),
    };
  }

  async function persist(draft) {
    setBusy(true, 'save');
    try {
      await bridgeInvoke('ecc.sync.dispatch-policy-save', {
        repo_root: state.repoRoot,
        excluded_modules: draft.excluded_modules,
        excluded_categories: draft.excluded_categories,
        excluded_skills: draft.excluded_skills,
        timeout_seconds: 120,
      });
      policyError('');
      return true;
    } catch (error) {
      policyError('策略没保存：' + reason(error) + '。开关已还原。');
      return false;
    } finally {
      setBusy(false);
    }
  }

  function describeTarget(kind, key) {
    if (kind === 'category') return '功能分类「' + categoryLabel(key) + '」';
    if (kind === 'module') return '上游模块「' + key + '」';
    return '技能「' + (skillNames[key] || key) + '」';
  }

  function targetCount(kind, key) {
    const report = state.report || {};
    if (kind === 'category') {
      const group = (report.categories || []).find((entry) => entry.key === key);
      return group ? int(group.total) : 0;
    }
    if (kind === 'module') {
      const group = (report.modules || []).find((entry) => entry.key === key);
      return group ? int(group.total) : 0;
    }
    return 1;
  }

  async function toggleRule(kind, key, exclude) {
    const draft = policyDraft();
    if (exclude) {
      const count = targetCount(kind, key);
      const hint = kind === 'skill'
        ? '这条技能不再发新版本，已发出去的版本保留在市场上。'
        : '这一类下 ' + count + ' 条技能不再发新版本，已发出去的版本保留在市场上。';
      const value = await askReason('排除 ' + describeTarget(kind, key), hint);
      if (value === null) return false;
      if (kind === 'category') draft.excluded_categories[key] = value;
      else if (kind === 'module') draft.excluded_modules[key] = value;
      else draft.excluded_skills[key] = value;
    } else {
      // 纳入会把一整类技能重新推回市场，不是随手可撤销的操作：先确认再落盘。
      const count = targetCount(kind, key);
      const hint = kind === 'skill'
        ? '这条技能会重新参与分发，下次同步时按上游最新版本发新版本。'
        : '这一类下 ' + count + ' 条技能会重新参与分发，下次同步时按上游最新版本发新版本。';
      const confirmed = await askConfirm(
        '纳入 ' + describeTarget(kind, key),
        hint,
        '策略写进 dispatch-policy.json 后随仓库长期生效，记得提交到仓库。',
      );
      if (!confirmed) return false;
      if (kind === 'category') delete draft.excluded_categories[key];
      else if (kind === 'module') delete draft.excluded_modules[key];
      else delete draft.excluded_skills[key];
    }
    const saved = await persist(draft);
    if (saved) {
      await reload();
      notify(
        (exclude ? '已排除 ' : '已纳入 ') + describeTarget(kind, key) +
          '，写入 ' + ((state.report && state.report.policy_file) || 'dispatch-policy.json') +
          '；提交到仓库后定时同步才会按它执行。',
        exclude ? 'warn' : 'ok',
      );
    }
    return saved;
  }

  // ---------- 确认弹窗（排除问原因、纳入问确认，共用一个壳） ----------

  let pendingModal = null;
  let modalNeedsInput = false;

  function openModal(options) {
    modalNeedsInput = !!options.input;
    el('modal-title').textContent = options.title;
    el('modal-desc').textContent = options.hint || '';
    el('modal-field').hidden = !modalNeedsInput;
    el('modal-input').value = modalNeedsInput ? options.value || '' : '';
    el('modal-hint').textContent = options.note || '';
    el('modal-hint').hidden = !options.note;
    el('modal-confirm').textContent = options.confirmLabel || '确定';
    el('modal').hidden = false;
    pendingModal = options.resolve;
    if (modalNeedsInput) {
      el('modal-input').focus();
      el('modal-input').select();
    } else {
      el('modal-confirm').focus();
    }
  }

  function askReason(title, hint) {
    return new Promise((resolve) => {
      openModal({
        title,
        hint,
        input: true,
        value: DEFAULT_REASON,
        note: '原因会写进 dispatch-policy.json，随仓库长期生效。',
        confirmLabel: '排除',
        resolve,
      });
    });
  }

  function askConfirm(title, hint, note) {
    return new Promise((resolve) => {
      openModal({ title, hint, input: false, note, confirmLabel: '纳入', resolve });
    });
  }

  function closeModal(value) {
    el('modal').hidden = true;
    const resolve = pendingModal;
    pendingModal = null;
    if (resolve) resolve(value);
  }

  function bindModal() {
    el('modal-form').addEventListener('submit', (event) => {
      event.preventDefault();
      closeModal(modalNeedsInput ? el('modal-input').value.trim() || DEFAULT_REASON : true);
    });
    el('modal-cancel').addEventListener('click', () => closeModal(null));
    el('modal').addEventListener('click', (event) => {
      if (event.target === el('modal')) closeModal(null);
    });
    document.addEventListener('keydown', (event) => {
      if (event.key === 'Escape' && !el('modal').hidden) closeModal(null);
    });
  }

  // ---------- 动作 ----------

  async function probeUpstream() {
    setBusy(true, 'probe');
    try {
      const result = await bridgeInvoke('ecc.sync.probe', {
        repo_root: state.repoRoot,
        timeout_seconds: 300,
      });
      const probe = (result && result.probe) || {};
      if (probe.changed) {
        notify(
          '上游有新提交 ' + text(probe.upstream_commit).slice(0, 8) + '，' +
            text(probe.reason || '需要搬运一次') + '。在 HiMind 里运行「ECC 技能同步」工作流即可拉取并按本页策略分发。',
          'warn',
        );
      } else {
        notify(
          '上游没有新提交，本地锁在 ' + text(probe.locked_commit || probe.upstream_commit).slice(0, 8) + '。',
          'ok',
        );
      }
    } catch (error) {
      notify('检查上游失败：' + reason(error), 'error');
    } finally {
      setBusy(false);
    }
  }

  async function runPublish(dryRun) {
    const limit = Math.max(0, int(el('limit').value));
    setBusy(true, dryRun ? 'dry-run' : 'publish');
    try {
      const result = await bridgeInvoke('ecc.sync.publish', {
        repo_root: state.repoRoot,
        dry_run: dryRun,
        limit: limit,
        allow_derived: false,
        timeout_seconds: 1800,
      });
      const publish = (result && result.publish) || {};
      const parts = [
        (dryRun ? '预演：' : '已发布：') + int(publish.published) + ' 条',
        '待发 ' + int(publish.pending) + ' 条',
      ];
      // 技能整批发一条 Release：「发了多少条」与「GitHub 上多了几条记录」是两个数，
      // 少了这一句，发一次同步看起来就像往仓库里塞了几百条 Release。
      const batches = Array.isArray(publish.batches) ? publish.batches : [];
      if (batches.length > 0) {
        parts.push('批次 Release ' + batches.length + ' 条');
      }
      if (int(publish.excluded)) parts.push('按策略跳过 ' + int(publish.excluded) + ' 条');
      if (int(publish.failed)) parts.push('失败 ' + int(publish.failed) + ' 条');
      parts.push('跳过校对 ' + ((publish.held_for_metadata_review || []).length) + ' 条');
      const blocked = int(publish.failed) > 0;
      notify(parts.join(' · '), blocked ? 'error' : dryRun ? '' : 'ok');
      await reload();
    } catch (error) {
      notify((dryRun ? '预演' : '发布') + '失败：' + reason(error), 'error');
    } finally {
      setBusy(false);
    }
  }

  function bindFilters() {
    const apply = () => {
      state.filters.category = el('filter-category').value;
      state.filters.module = el('filter-module').value;
      state.filters.keyword = el('filter-keyword').value.trim();
      reload();
    };
    el('filter-category').addEventListener('change', apply);
    el('filter-module').addEventListener('change', apply);
    let timer = 0;
    el('filter-keyword').addEventListener('input', () => {
      window.clearTimeout(timer);
      timer = window.setTimeout(apply, 260);
    });
    el('filter-reset').addEventListener('click', () => {
      el('filter-keyword').value = '';
      state.filters = { state: '', category: '', module: '', keyword: '' };
      reload();
    });
    // 计数条就是状态筛选入口：点一下只看这一类，再点一下回到全部。
    el('summary').addEventListener('click', (event) => {
      const chip = event.target.closest ? event.target.closest('button[data-state]') : null;
      if (!chip) return;
      const key = chip.dataset.state;
      state.filters.state = state.filters.state === key ? '' : key;
      reload();
    });
  }

  function bindPolicy() {
    el('policy-columns').addEventListener('change', async (event) => {
      const input = event.target.closest ? event.target.closest('input[data-kind]') : null;
      if (!input || state.busy) return;
      const kind = input.dataset.kind;
      const key = input.dataset.key;
      const exclude = !input.checked;
      input.disabled = true;
      const saved = await toggleRule(kind, key, exclude);
      if (!saved) {
        // 没落盘就按服务端最后一份策略重画，别把界面留在「看起来已经改了」的状态。
        if (state.report) renderPolicy(state.report);
        else {
          input.checked = !input.checked;
          input.disabled = false;
        }
      }
    });
  }

  function bindToolbar() {
    el('refresh').addEventListener('click', () => {
      if (!state.busy) reload();
    });
    el('repo-use').addEventListener('click', useTypedRepoRoot);
    el('repo').addEventListener('input', syncRepoUse);
    el('repo').addEventListener('change', syncRepoUse);
    el('repo').addEventListener('keydown', (event) => {
      if (event.key === 'Enter') useTypedRepoRoot();
    });
    el('dry-run').addEventListener('click', () => runPublish(true));
    el('publish').addEventListener('click', () => runPublish(false));
    el('action-probe').addEventListener('click', () => {
      if (!state.busy) probeUpstream();
    });
  }

  showTab('skills');
  bindTabs();
  bindToolbar();
  bindFilters();
  bindSkillList();
  bindDetail();
  bindPolicy();
  bindModal();
  boot();
})();
