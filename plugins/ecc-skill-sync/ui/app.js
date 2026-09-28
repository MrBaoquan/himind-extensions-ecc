/**
 * ECC 分发管理自带视图。
 *
 * 视图只消费本插件 Manifest 已声明的能力：读分发状态、写分发策略、探测上游、
 * 发布待发项。它不直接读写文件，也不拼 shell 命令，因此插件被单独打开或
 * 嵌入宿主时行为一致。
 *
 * 分发策略的判断全部落在 Go 侧：界面只回传「哪些分类/模块/单条技能被排除、
 * 各自的原因」，判定、优先级和落盘校验由 eccsync 负责。界面自算一套口径，
 * 迟早会出现「界面说会发、定时任务却跳过」这种最难查的不一致。
 */
(function () {
  'use strict';

  const DEFAULT_REASON = '按分发策略排除';
  const STORE_KEY = 'com.mrbaoquan.ecc-skill-sync.repo_root';
  const MAX_ITEMS = 500;

  const STATE_LABEL = {
    distributed: '已分发',
    pending: '待发布',
    held: '待校对',
    excluded: '已排除',
  };
  const STATE_TONE = {
    distributed: 'ok',
    pending: '',
    held: 'warn',
    excluded: 'danger',
  };

  const state = {
    repoRoot: '',
    report: null,
    skillNames: {},
    filters: { state: '', category: '', module: '', keyword: '' },
    busy: false,
  };

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

  function setBusy(busy, label) {
    state.busy = busy;
    ['refresh', 'probe', 'dry-run', 'publish', 'repo-use'].forEach((id) => {
      el(id).disabled = busy;
    });
    el('publish').textContent = busy && label ? label : '发布待发项';
    el('dry-run').textContent = busy && label ? '执行中…' : '预演发布';
  }

  function reason(error) {
    if (!error) return '未知错误';
    if (typeof error === 'string') return error;
    return error.message || JSON.stringify(error);
  }

  // ---------- 仓库根目录 ----------

  function storedRepoRoot() {
    try {
      return window.localStorage.getItem(STORE_KEY) || '';
    } catch (error) {
      return '';
    }
  }

  function rememberRepoRoot(path) {
    try {
      window.localStorage.setItem(STORE_KEY, path);
    } catch (error) {
      /* 存不住只影响下次自动填充，不影响本次使用 */
    }
  }

  function forgetRepoRoot() {
    try {
      window.localStorage.removeItem(STORE_KEY);
    } catch (error) {
      /* 清不掉只影响下次自动填充，不影响本次使用 */
    }
  }

  function adoptRepoRoot(path) {
    state.repoRoot = path;
    el('repo').value = path;
    el('policy-foot').textContent = path;
    el('policy-foot').title = path;
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
      const response = await bridgeInvoke('ecc.sync.repo_hint', {
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

  // 该用哪个目录：记住的那个确实还能用就继续用，否则换成宿主推断出来的第一个。
  // 推断不可用时才退回旧行为（记住的路径 → 当前工作区），老 Agent 上照常能用。
  function chooseRepoRoot(stored, hint, workspace) {
    if (hint && hint.current && hint.current.valid) return { path: stored, source: 'stored' };
    const usable = usableCandidates(hint);
    if (usable.length) return { path: usable[0].path, source: 'host' };
    if (hint) return { path: '', source: '' };
    if (stored) return { path: stored, source: 'stored' };
    return { path: workspace || '', source: workspace ? 'workspace' : '' };
  }

  function isRepoPathError(error) {
    const message = reason(error);
    return message.indexOf('不是 ECC 同步仓库') >= 0 || message.indexOf('需要 repo_root') >= 0;
  }

  // 手里的路径已经不能用了：换一个候选，换不动就把记忆清掉，
  // 免得下次打开又从一个错的路径开始。
  async function recoverRepoRoot() {
    const hint = await repoHint(state.repoRoot);
    const next = usableCandidates(hint).find((item) => !samePath(item.path, state.repoRoot));
    if (!next) return '';
    adoptRepoRoot(next.path);
    return next.path;
  }

  async function boot() {
    const stored = storedRepoRoot();
    const hint = await repoHint(stored);
    const workspace = hint ? '' : await workspaceFromHost();
    const choice = chooseRepoRoot(stored, hint, workspace);
    if (!choice.path) {
      notify('没找到 ECC 仓库，请填写 himind-extensions-ecc 仓库根目录。', 'warn');
      el('repo').focus();
      return;
    }
    adoptRepoRoot(choice.path);
    if (choice.source !== 'stored') forgetRepoRoot();
    const candidates = usableCandidates(hint).length;
    const note = choice.source === 'host'
      ? '已自动定位 ECC 仓库：' + choice.path +
        (candidates > 1 ? '（本机另有 ' + (candidates - 1) + ' 个候选，可在上方改写）' : '')
      : '';
    await reload(note);
  }

  async function useTypedRepoRoot() {
    const path = el('repo').value.trim();
    if (!path) {
      notify('请填写仓库根目录。', 'warn');
      return;
    }
    adoptRepoRoot(path);
    await reload();
  }

  // ---------- 读取与渲染 ----------

  async function reload(note) {
    if (!state.repoRoot) {
      notify('先填上 himind-extensions-ecc 仓库根目录，再进入分发管理。', 'warn');
      return;
    }
    notify('');
    setBusy(true, '读取中…');
    try {
      const report = await bridgeInvoke('ecc.sync.distribution', {
        repo_root: state.repoRoot,
        state: state.filters.state,
        category: state.filters.category,
        module: state.filters.module,
        keyword: state.filters.keyword,
        limit: MAX_ITEMS,
        timeout_seconds: 120,
      });
      state.report = report;
      rememberRepoRoot(state.repoRoot);
      (report.items || []).forEach((item) => {
        if (item.slug && item.name) state.skillNames[item.slug] = item.name;
      });
      render();
      notify(note || '', note ? 'ok' : '');
    } catch (error) {
      if (isRepoPathError(error)) {
        const recovered = await recoverRepoRoot();
        if (recovered) return reload('已自动改用 ' + recovered);
        forgetRepoRoot();
      }
      notify('读取分发状态失败：' + reason(error), 'error');
    } finally {
      setBusy(false);
    }
  }

  function render() {
    const report = state.report;
    if (!report) return;
    renderBrandSub(report);
    renderStats(report);
    renderFilters(report);
    renderRail(report);
    renderItems(report);
  }

  function renderBrandSub(report) {
    const commit = text(report.upstream_commit).slice(0, 8);
    const parts = [];
    if (report.repository) parts.push(report.repository);
    if (commit) parts.push('上游 ' + commit);
    if (report.commit_date) parts.push(text(report.commit_date).slice(0, 10));
    if (report.sequence) parts.push('第 ' + report.sequence + ' 次同步');
    el('brand-sub').textContent = parts.join(' · ') || '上游技能的分发策略与状态';
  }

  function renderStats(report) {
    const totals = report.totals || {};
    const cards = [
      { label: '已收录技能', value: int(totals.skills) },
      { label: '已分发', value: int(totals.distributed), tone: 'ok' },
      { label: '待发布', value: int(totals.pending), tone: 'accent' },
      { label: '待校对', value: int(totals.held), tone: 'warn' },
      {
        label: '已排除',
        value: int(totals.excluded),
        tone: 'danger',
        sub: int(totals.frozen) ? '其中 ' + int(totals.frozen) + ' 条停更' : '',
      },
      { label: '已发版本', value: int(totals.published_versions) },
    ];
    el('stats').innerHTML = cards
      .map((card) => {
        const sub = card.sub ? ' <small>' + escapeHTML(card.sub) + '</small>' : '';
        return (
          '<div class="stat ' + (card.tone || '') + '">' +
          '<span class="value">' + escapeHTML(card.value) + sub + '</span>' +
          '<span class="label">' + escapeHTML(card.label) + '</span>' +
          '</div>'
        );
      })
      .join('');
  }

  function renderFilters(report) {
    fillSelect(el('filter-category'), (report.categories || []).map((group) => group.key), '全部分类');
    fillSelect(el('filter-module'), (report.modules || []).map((group) => group.key), '全部模块');
    el('filter-state').value = state.filters.state;
    el('filter-category').value = state.filters.category;
    el('filter-module').value = state.filters.module;
    el('filter-keyword').value = state.filters.keyword;
  }

  function fillSelect(select, values, placeholder) {
    const current = select.value;
    const options = ['<option value="">' + escapeHTML(placeholder) + '</option>']
      .concat(
        values.map((value) => '<option value="' + escapeHTML(value) + '">' + escapeHTML(value) + '</option>'),
      )
      .join('');
    select.innerHTML = options;
    if (values.indexOf(current) >= 0) select.value = current;
  }

  function renderRail(report) {
    const policy = report.policy || {};
    const excludedModules = policy.excluded_modules || {};
    const excludedCategories = policy.excluded_categories || {};
    const excludedSkills = policy.excluded_skills || {};
    const ruleCount =
      Object.keys(excludedModules).length +
      Object.keys(excludedCategories).length +
      Object.keys(excludedSkills).length;
    el('policy-count').textContent = ruleCount ? ruleCount + ' 条规则' : '全部分发';
    el('policy-foot').textContent = report.policy_path || state.repoRoot;
    el('policy-foot').title = report.policy_path || state.repoRoot;

    el('category-list').innerHTML = (report.categories || [])
      .map((group) => {
        const direct = excludedCategories[group.key] !== undefined;
        const item = ruleItem({
          kind: 'category',
          key: group.key,
          total: int(group.total),
          excluded: int(group.excluded),
          checked: !direct,
          reason: direct ? excludedCategories[group.key] : '',
          tone: direct ? 'off' : '',
        });
        return item;
      })
      .join('') || '<li class="empty">暂无分类</li>';

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
              ? '随分类 ' + group.category + ' 排除'
              : '',
          tone: direct ? 'off' : inherited ? 'inherited' : '',
        });
      })
      .join('') || '<li class="empty">暂无模块</li>';

    const skillKeys = Object.keys(excludedSkills);
    el('skill-rule-list').innerHTML =
      skillKeys.map((slug) => ruleItem({
        kind: 'skill',
        key: slug,
        total: 1,
        excluded: 1,
        checked: false,
        reason: excludedSkills[slug],
        tone: 'off',
        label: state.skillNames[slug] || slug,
      })).join('');
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
      '</span></li>'
    );
  }

  function renderItems(report) {
    const items = report.items || [];
    if (!items.length) {
      el('items-body').innerHTML =
        '<tr><td colspan="6"><p class="row-empty">没有符合条件的技能。换个筛选条件试试。</p></td></tr>';
    } else {
      el('items-body').innerHTML = items.map(itemRow).join('');
    }
    const truncated = int(report.truncated);
    el('table-foot').textContent = truncated
      ? '显示前 ' + items.length + ' 条，另有 ' + truncated + ' 条未展开。'
      : '共 ' + items.length + ' 条。';
  }

  function itemRow(item) {
    const stateKey = text(item.state);
    const badge =
      '<span class="badge ' + (STATE_TONE[stateKey] || '') + '">' +
      escapeHTML(STATE_LABEL[stateKey] || stateKey) +
      '</span>';
    const why = item.detail
      ? '<span class="why" title="' + escapeHTML(item.detail) + '">' + escapeHTML(item.detail) + '</span>'
      : '';
    const ruleHint = item.rule
      ? '<span class="why">命中规则：' + escapeHTML(ruleText(item.rule, item.keyword)) + '</span>'
      : '';
    const excluding = stateKey === 'excluded';
    const categories = (item.categories || [])
      .map((value) => '<span class="badge">' + escapeHTML(value) + '</span>')
      .join('');
    return (
      '<tr>' +
      '<td><div class="skill-name"><strong>' + escapeHTML(item.name || item.slug) + '</strong>' +
      '<code title="' + escapeHTML(item.source_path) + '">' + escapeHTML(item.source_path || item.slug) + '</code>' +
      '</div></td>' +
      '<td><div class="tag-list">' + (categories || '<span class="badge">未归类</span>') + '</div></td>' +
      '<td>' + escapeHTML(item.module || '—') + '</td>' +
      '<td>' + escapeHTML(item.version) + (item.tier ? ' <span class="badge">' + escapeHTML(item.tier) + '</span>' : '') + '</td>' +
      '<td><div class="detail-cell">' + badge + why + ruleHint + '</div></td>' +
      '<td class="col-action">' +
      '<button class="btn" type="button" data-slug="' + escapeHTML(item.slug) + '"' +
      ' data-action="' + (excluding ? 'include' : 'exclude') + '">' +
      (excluding ? '纳入' : '排除') +
      '</button></td></tr>'
    );
  }

  function ruleText(rule, keyword) {
    const names = { skill: '单条技能', module: '上游模块', category: '功能分类' };
    return (names[rule] || rule) + (keyword ? '「' + keyword + '」' : '');
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
    setBusy(true, '保存中…');
    try {
      await bridgeInvoke('ecc.sync.dispatch_policy_save', {
        repo_root: state.repoRoot,
        excluded_modules: draft.excluded_modules,
        excluded_categories: draft.excluded_categories,
        excluded_skills: draft.excluded_skills,
        timeout_seconds: 120,
      });
      return true;
    } catch (error) {
      notify('策略没保存：' + reason(error), 'error');
      return false;
    } finally {
      setBusy(false);
    }
  }

  function describeTarget(kind, key) {
    if (kind === 'category') return '功能分类「' + key + '」';
    if (kind === 'module') return '上游模块「' + key + '」';
    return '技能「' + (state.skillNames[key] || key) + '」';
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
      if (kind === 'category') delete draft.excluded_categories[key];
      else if (kind === 'module') delete draft.excluded_modules[key];
      else delete draft.excluded_skills[key];
    }
    const saved = await persist(draft);
    if (saved) {
      await reload();
      notify(
        (exclude ? '已排除 ' : '已纳入 ') + describeTarget(kind, key) +
          '。策略写在 ' + ((state.report && state.report.policy_file) || 'dispatch-policy.json') +
          '，记得提交到仓库——定时同步读的是同一份文件。',
        exclude ? 'warn' : 'ok',
      );
    }
    return saved;
  }

  // ---------- 排除原因弹窗 ----------

  let pendingReason = null;

  function askReason(title, hint) {
    return new Promise((resolve) => {
      el('modal-title').textContent = title;
      el('modal-desc').textContent = hint || '';
      el('modal-input').value = DEFAULT_REASON;
      el('modal').hidden = false;
      pendingReason = resolve;
      el('modal-input').focus();
      el('modal-input').select();
    });
  }

  function closeModal(value) {
    el('modal').hidden = true;
    const resolve = pendingReason;
    pendingReason = null;
    if (resolve) resolve(value);
  }

  function bindModal() {
    el('modal-form').addEventListener('submit', (event) => {
      event.preventDefault();
      const value = el('modal-input').value.trim() || DEFAULT_REASON;
      closeModal(value);
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
    setBusy(true, '探测中…');
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
    setBusy(true, dryRun ? '预演中…' : '发布中…');
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
      state.filters = {
        state: el('filter-state').value,
        category: el('filter-category').value,
        module: el('filter-module').value,
        keyword: el('filter-keyword').value.trim(),
      };
      reload();
    };
    el('filter-state').addEventListener('change', apply);
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
  }

  function bindRail() {
    el('rail-body').addEventListener('change', async (event) => {
      const input = event.target.closest('input[data-kind]');
      if (!input || state.busy) return;
      const kind = input.dataset.kind;
      const key = input.dataset.key;
      const exclude = !input.checked;
      input.disabled = true;
      const saved = await toggleRule(kind, key, exclude);
      if (!saved) {
        input.checked = !input.checked;
        input.disabled = false;
      }
    });
  }

  function bindItems() {
    el('items-body').addEventListener('click', async (event) => {
      const button = event.target.closest('button[data-action]');
      if (!button || state.busy) return;
      button.disabled = true;
      const saved = await toggleRule('skill', button.dataset.slug, button.dataset.action === 'exclude');
      if (!saved) button.disabled = false;
    });
  }

  function bindToolbar() {
    el('repo-use').addEventListener('click', useTypedRepoRoot);
    el('repo').addEventListener('keydown', (event) => {
      if (event.key === 'Enter') useTypedRepoRoot();
    });
    el('refresh').addEventListener('click', reload);
    el('probe').addEventListener('click', probeUpstream);
    el('dry-run').addEventListener('click', () => runPublish(true));
    el('publish').addEventListener('click', () => runPublish(false));
  }

  bindToolbar();
  bindFilters();
  bindRail();
  bindItems();
  bindModal();
  boot();
})();
