const SURFACE_FNS = { convex_squircle: (x) => Math.pow(1 - Math.pow(1 - x, 4), 0.25) };
    function calcRefractionProfile(t, b, hf, ior, s) {
      s = s||128; const eta = 1/ior; const p = new Float64Array(s);
      function refr(nx,ny) { const d = ny, k = 1 - eta*eta*(1 - d*d); if (k < 0) return null; const sq = Math.sqrt(k); return [-(eta*d+sq)*nx, eta-(eta*d+sq)*ny]; }
      for (let i=0;i<s;i++) { const x=i/s, y=hf(x), dx=x<1?.0001:-.0001, y2=hf(x+dx), deriv=(y2-y)/dx, mag=Math.sqrt(deriv*deriv+1), ref=refr(-deriv/mag,-1/mag); if (!ref) {p[i]=0;continue;} p[i]=ref[0]*((y*b+t)/ref[1]); }
      return p;
    }
    function genDispMap(w,h,r,bw,prof,maxD) {
      const c=document.createElement('canvas'); c.width=w; c.height=h; const ctx=c.getContext('2d'), img=ctx.createImageData(w,h), d=img.data;
      for (let i=0;i<d.length;i+=4){d[i]=128;d[i+1]=128;d[i+2]=0;d[i+3]=255;}
      const rSq=r*r,r1Sq=(r+1)**2,rBSq=Math.max(r-bw,0)**2,wB=w-r*2,hB=h-r*2,S=prof.length;
      for (let y1=0;y1<h;y1++)for(let x1=0;x1<w;x1++){const x=x1<r?x1-r:x1>=w-r?x1-r-wB:0, y=y1<r?y1-r:y1>=h-r?y1-r-hB:0, dSq=x*x+y*y; if(dSq>r1Sq||dSq<rBSq)continue; const dist=Math.sqrt(dSq), fromSide=r-dist, op=dSq<rSq?1:1-(dist-Math.sqrt(rSq))/(Math.sqrt(r1Sq)-Math.sqrt(rSq)); if(op<=0||dist===0)continue; const cos=x/dist, sin=y/dist, bi=Math.min(((fromSide/bw)*S)|0,S-1), disp=prof[bi]||0, dX=(-cos*disp)/maxD, dY=(-sin*disp)/maxD, idx=(y1*w+x1)*4; d[idx]=(128+dX*127*op+.5)|0; d[idx+1]=(128+dY*127*op+.5)|0;}
      ctx.putImageData(img,0,0); return c.toDataURL();
    }
    function genSpecMap(w,h,r,bw,angle) {
      angle=angle!=null?angle:Math.PI/3; const c=document.createElement('canvas'); c.width=w;c.height=h; const ctx=c.getContext('2d'), img=ctx.createImageData(w,h), d=img.data; d.fill(0);
      const rSq=r*r,r1Sq=(r+1)**2,rBSq=Math.max(r-bw,0)**2,wB=w-r*2,hB=h-r*2,sv=[Math.cos(angle),Math.sin(angle)];
      for(let y1=0;y1<h;y1++)for(let x1=0;x1<w;x1++){const x=x1<r?x1-r:x1>=w-r?x1-r-wB:0,y=y1<r?y1-r:y1>=h-r?y1-r-hB:0,dSq=x*x+y*y;if(dSq>r1Sq||dSq<rBSq)continue;const dist=Math.sqrt(dSq),fromSide=r-dist,op=dSq<rSq?1:1-(dist-Math.sqrt(rSq))/(Math.sqrt(r1Sq)-Math.sqrt(rSq));if(op<=0||dist===0)continue;const cos=x/dist,sin=-y/dist,dot=Math.abs(cos*sv[0]+sin*sv[1]),edge=Math.sqrt(Math.max(0,1-(1-fromSide)**2)),coeff=dot*edge,col=(255*coeff)|0,alpha=(col*coeff*op)|0,idx=(y1*w+x1)*4;d[idx]=col;d[idx+1]=col;d[idx+2]=col;d[idx+3]=alpha;}
      ctx.putImageData(img,0,0); return c.toDataURL();
    }
    function initLiquidGlass() {
      /* Redesigned: cards use CSS backdrop-filter, glass refraction removed */
    }

    let proxyData = null, enabledModels = [], allDisplayNames = {}, editingIdx = -1, umansUserId = '';
    let disabledModels = new Set();

    function showToast(m, type) { const d=document.createElement('div'); d.className=`toast align-items-center text-white bg-${type||'info'} border-0 show`; d.innerHTML=`<div class="d-flex"><div class="toast-body"></div><button type="button" class="btn-close btn-close-white me-2 m-auto" data-bs-dismiss="toast"></button></div>`; d.querySelector('.toast-body').textContent = m; document.getElementById('toastContainer').appendChild(d); setTimeout(()=>d.remove(),3000); }
    function escapeHtml(t){if(t==null)return '';const d=document.createElement('div');d.textContent=String(t);return d.innerHTML.replace(/"/g,'&quot;').replace(/'/g,'&#39;');}
    function layoutStatGrids() {
      const minColWidth = 120;
      document.querySelectorAll('.stat-grid').forEach(grid => {
        const n = grid.children.length;
        if (!n) return;
        const w = grid.clientWidth;
        let best = 1;
        for (let d = n; d >= 1; d--) {
          if (n % d === 0 && w / d >= minColWidth) { best = d; break; }
        }
        grid.style.gridTemplateColumns = `repeat(${best}, 1fr)`;
      });
    }
    function toggleSection(n) {
      const b=document.getElementById('section-'+n),
            c=document.querySelector('[data-section="'+n+'"]'),
            i=c?.querySelector('.toggle-icon'),
            h=c?.querySelector('.collapsible-header');
      if(!b||!i) return;
      // Cancel any in-progress transition cleanup
      b.style.height='';
      b.style.display='';
      const isCollapsed=b.classList.contains('collapsed');
      if(isCollapsed){
        // Expand: make visible, measure, animate from 0
        b.style.display='';
        b.classList.remove('collapsed');
        i.classList.remove('collapsed');
        if(h) h.setAttribute('aria-expanded','true');
        i.classList.remove('bi-chevron-right');
        i.classList.add('bi-chevron-down');
        const target=b.scrollHeight+'px';
        b.style.height='0px';
        b.offsetHeight; // force reflow
        b.style.height=target;
      } else {
        // Collapse: animate to 0, then display:none
        b.style.height=b.scrollHeight+'px';
        b.offsetHeight; // force reflow to lock start height
        b.style.height='0px';
        b.classList.add('collapsed');
        i.classList.add('collapsed');
        if(h) h.setAttribute('aria-expanded','false');
        i.classList.remove('bi-chevron-down');
        i.classList.add('bi-chevron-right');
      }
      // Clear inline height after transition completes; hide after collapse
      let done=false;
      const cleanup=()=>{
        if(done) return;
        done=true;
        b.style.height='';
        b.removeEventListener('transitionend',onEnd);
        if(b.classList.contains('collapsed')) { b.style.display='none'; }
      };
      const onEnd=(e)=>{ if(e.propertyName!=='height') return; cleanup(); };
      b.addEventListener('transitionend',onEnd);
      setTimeout(cleanup,350);
      setTimeout(initLiquidGlass,50);
      setTimeout(layoutStatGrids,50);
    }

    function setWallpaperActive(source) {
      document.querySelectorAll('.btn-wp').forEach(b => { const isActive = b.dataset.wp === source; b.classList.toggle('active', isActive); b.setAttribute('aria-pressed', isActive ? 'true' : 'false'); });
    }
    let _concurrencyLimitMode = 'soft';
    let _manualLimit = 1;
    function setConcurrencyLimitActive(mode) {
      _concurrencyLimitMode = mode;
      document.querySelectorAll('.btn-conclimit').forEach(btn => {
        const isActive = btn.dataset.conclimit === mode;
        btn.classList.toggle('active', isActive);
        btn.setAttribute('aria-pressed', isActive ? 'true' : 'false');
      });
      const wrapper = document.getElementById('manualLimitWrapper');
      if (wrapper) wrapper.style.display = mode === 'manual' ? '' : 'none';
      if (mode === 'manual') {
        const slider = document.getElementById('manualLimitSlider');
        const valEl = document.getElementById('manualLimitValue');
        if (slider && _manualLimit > 0) { slider.value = _manualLimit; }
        if (valEl) { valEl.textContent = _manualLimit > 0 ? _manualLimit : '--'; }
      }
    }
    async function setConcurrencyLimitMode(mode) {
      const prev = _concurrencyLimitMode;
      setConcurrencyLimitActive(mode);
      try {
        const r = await fetch('/api/config', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ concurrencyLimitMode: mode }) });
        if (!r.ok) throw new Error('config save failed');
        fetchConcurrency();
      } catch {
        setConcurrencyLimitActive(prev);
      }
    }
    function onManualLimitInput(value) {
      document.getElementById('manualLimitValue').textContent = value;
    }
    async function onManualLimitChange(value) {
      const limit = parseInt(value, 10);
      const prev = _manualLimit;
      _manualLimit = limit;
      document.getElementById('manualLimitValue').textContent = limit;
      try {
        const r = await fetch('/api/config', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ manualConcurrencyLimit: limit }) });
        if (!r.ok) throw new Error('config save failed');
        fetchConcurrency();
      } catch {
        _manualLimit = prev;
        const slider = document.getElementById('manualLimitSlider');
        if (slider) slider.value = prev;
        document.getElementById('manualLimitValue').textContent = prev;
      }
    }
    function onSlotFreeDelayInput(value) {
      const v = parseInt(value, 10) || 0;
      const valEl = document.getElementById('slotFreeDelayValue');
      if (valEl) valEl.textContent = v + 's';
    }
    async function onSlotFreeDelayChange(value) {
      const delay = Math.max(0, Math.min(60, parseInt(value, 10) || 0));
      const slider = document.getElementById('slotFreeDelaySlider');
      try {
        const r = await fetch('/api/config', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ slotFreeDelay: delay }) });
        if (!r.ok) throw new Error('config save failed');
        if (slider) slider.value = delay;
        const valEl = document.getElementById('slotFreeDelayValue');
        if (valEl) valEl.textContent = delay + 's';
      } catch {
        // Revert handled by next loadConfig/fetchConcurrency
      }
    }
    // ─── Retry / Backoff / Timeout settings ───
    const BACKOFF_PRESETS = {{.BackoffPresets}};
    const DEFAULT_RETRY_ATTEMPTS = {{.DefaultRetryAttempts}};
    const DEFAULT_REQUEST_TIMEOUT = {{.DefaultRequestTimeout}};
    const PROXY_VERSION = "{{.Version}}";
    function formatSeconds(s) {
      if (s < 60) return s + 's';
      if (s % 60 === 0) return (s / 60) + 'm';
      return Math.floor(s / 60) + 'm' + (s % 60) + 's';
    }
    function updateBackoffPreview(retries, strategy) {
      const el = document.getElementById('backoffPreview');
      if (!el) return;
      if (retries <= 0) { el.textContent = ''; return; }
      const preset = BACKOFF_PRESETS[strategy] || BACKOFF_PRESETS.aggressive;
      const delays = [];
      for (let i = 0; i < retries; i++) {
        delays.push(formatSeconds(preset[i < preset.length ? i : preset.length - 1]));
      }
      el.textContent = delays.join(' → ');
    }
    function updateBackoffVisibility(retries) {
      const wrapper = document.getElementById('backoffStrategyWrapper');
      if (wrapper) wrapper.style.display = retries > 0 ? '' : 'none';
    }
    function onRetryAttemptsInput(value) {
      const v = parseInt(value, 10) || 0;
      document.getElementById('retryAttemptsValue').textContent = v;
      updateBackoffVisibility(v);
      updateBackoffPreview(v, document.getElementById('backoffStrategySelect')?.value || 'aggressive');
    }
    async function onRetryAttemptsChange(value) {
      const attempts = Math.max(0, Math.min(10, parseInt(value, 10) || 0));
      const slider = document.getElementById('retryAttemptsSlider');
      try {
        const r = await fetch('/api/config', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ retryAttempts: attempts }) });
        if (!r.ok) throw new Error('config save failed');
      } catch {
        // Revert on failure — next loadConfig will resync
        loadConfig();
      }
    }
    async function onBackoffStrategyChange(value) {
      const select = document.getElementById('backoffStrategySelect');
      const prev = select.dataset.prev || 'aggressive';
      select.dataset.prev = value;
      try {
        const r = await fetch('/api/config', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ backoffStrategy: value }) });
        if (!r.ok) throw new Error('config save failed');
        const retries = parseInt(document.getElementById('retryAttemptsSlider')?.value || '0', 10);
        updateBackoffPreview(retries, value);
      } catch {
        select.value = prev;
      }
    }
    function onRequestTimeoutInput(value) {
      const secs = parseInt(value, 10) || DEFAULT_REQUEST_TIMEOUT;
      document.getElementById('requestTimeoutValue').textContent = formatSeconds(secs);
    }
    async function onRequestTimeoutChange(value) {
      const secs = Math.max(30, Math.min(1800, parseInt(value, 10) || DEFAULT_REQUEST_TIMEOUT));
      const slider = document.getElementById('requestTimeoutSlider');
      const valEl = document.getElementById('requestTimeoutValue');
      const prev = parseInt(slider.dataset.prev || String(DEFAULT_REQUEST_TIMEOUT), 10);
      slider.dataset.prev = secs;
      valEl.textContent = formatSeconds(secs);
      try {
        const r = await fetch('/api/config', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ requestTimeout: secs }) });
        if (!r.ok) throw new Error('config save failed');
      } catch {
        slider.value = prev;
        valEl.textContent = formatSeconds(prev);
      }
    }
    function setApiKeyModeActive(mode) {
      document.querySelectorAll('.btn-apikeymode').forEach(btn => {
        const isActive = btn.dataset.apikeymode === mode;
        btn.classList.toggle('active', isActive);
        btn.setAttribute('aria-pressed', isActive ? 'true' : 'false');
      });
      const tokensCard = document.querySelector('[data-section="tokens"]');
      if (tokensCard) tokensCard.style.display = mode === 'passthrough' ? 'none' : '';
    }
    async function toggleApiKeyMode(mode) {
      const prevMode = document.querySelector('.btn-apikeymode.active')?.dataset.apikeymode || 'smart';
      setApiKeyModeActive(mode);
      try {
        await fetch('/api/config', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ apikeyMode: mode }) });
      } catch (e) {
        setApiKeyModeActive(prevMode);
      }
    }
    let _wallpaperObjUrl = null;
    function applyWallpaperBlob(blob) {
      if (_wallpaperObjUrl) { try { URL.revokeObjectURL(_wallpaperObjUrl); } catch {} _wallpaperObjUrl = null; }
      _wallpaperObjUrl = URL.createObjectURL(blob);
      document.body.style.backgroundImage = `url("${_wallpaperObjUrl}")`;
      document.body.style.backgroundRepeat='no-repeat';
      document.body.style.backgroundPosition='center';
      document.body.style.backgroundSize='cover';
      document.body.style.backgroundAttachment='fixed';
      document.body.style.backgroundColor='#0d1117';
      document.body.classList.add('has-wallpaper');
    }
    function clearWallpaper() {
      if (_wallpaperObjUrl) { try { URL.revokeObjectURL(_wallpaperObjUrl); } catch {} _wallpaperObjUrl = null; }
      document.body.style.setProperty('background-image', 'none', 'important');
      document.body.style.setProperty('background-color', '#0d1117', 'important');
      document.body.classList.remove('has-wallpaper');
    }
    async function setWallpaper(source, skipConfigSave) {
      setWallpaperActive(source);
      if (source === 'none') {
        clearWallpaper();
      } else if (source === 'bing') {
        try { const r = await fetch('/api/bg'); if (!r.ok) throw Error(); const blob = await r.blob(); applyWallpaperBlob(blob); } catch { clearWallpaper(); }
      } else if (source === 'wallhaven') {
        try { const r = await fetch('/api/bg-wallhaven'); if (!r.ok) throw Error(); const blob = await r.blob(); applyWallpaperBlob(blob); } catch { clearWallpaper(); }
      }
      if (!skipConfigSave) {
        try { await fetch('/api/config',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({wallpaperSource:source})}); } catch {}
      }
    }
    function setVisionHandoffActive(enabled) {
      document.querySelectorAll('.btn-handoff').forEach(btn => {
        const isActive = (enabled && btn.dataset.handoff === 'on') || (!enabled && btn.dataset.handoff === 'off');
        btn.classList.toggle('active', isActive);
        btn.setAttribute('aria-pressed', isActive ? 'true' : 'false');
      });
    }
    async function toggleVisionHandoff(enabled) {
      setVisionHandoffActive(enabled);
      const cacheWrapper = document.getElementById('handoffCacheWrapper');
      if (cacheWrapper) cacheWrapper.style.display = enabled ? '' : 'none';
      try {
        await fetch('/api/config', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ visionHandoffEnabled: enabled }) });
      } catch (e) {
        setVisionHandoffActive(!enabled);
        if (cacheWrapper) cacheWrapper.style.display = !enabled ? '' : 'none';
      }
    }

    function setHandoffCacheActive(enabled) {
      document.querySelectorAll('.btn-handoffcache').forEach(btn => {
        const isActive = (enabled && btn.dataset.handoffcache === 'on') || (!enabled && btn.dataset.handoffcache === 'off');
        btn.classList.toggle('active', isActive);
        btn.setAttribute('aria-pressed', isActive ? 'true' : 'false');
      });
    }
    async function toggleHandoffCache(enabled) {
      setHandoffCacheActive(enabled);
      try {
        await fetch('/api/config', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ visionHandoffCacheEnabled: enabled }) });
      } catch (e) {
        setHandoffCacheActive(!enabled);
      }
    }

    function renderTokenPools() {
      const container = document.getElementById('tokenPoolsContainer');
      if (!proxyData || !proxyData.token_state || !proxyData.token_state.length) {
        container.innerHTML = '<div class="text-center py-3"><i class="bi bi-key" style="font-size:2rem;color:rgba(255,255,255,0.7)"></i><p class="mt-2 text-muted">' + escapeHtml('No API keys') + '</p></div>';
        document.getElementById('tokenCountBadge').textContent = '0'; return;
      }
      document.getElementById('tokenCountBadge').textContent = proxyData.token_state.length;
      container.innerHTML = proxyData.token_state.map(p => {
        const isOn = p.status === 'active';
        return `<div class="pool-card"><div class="pool-header"><div class="pool-name">${escapeHtml(p.name)}</div><span class="badge rounded-pill" style="background:${isOn?'rgba(52,211,153,0.2)':'rgba(248,113,113,0.2)'};color:${isOn?'#34d399':'#f87171'};font-size:0.7rem;font-weight:600;border:1px solid ${isOn?'rgba(52,211,153,0.3)':'rgba(248,113,113,0.3)'}">${isOn?'Active':'Inactive'}</span></div><div style="font-size:0.85rem;color:rgba(255,255,255,0.7)">${escapeHtml(p.token)}</div></div>`;
      }).join('');
    }

    function renderModels() {
      const container = document.getElementById('modelsContainer');
      const badge = document.getElementById('modelCountBadge');
      if (!enabledModels.length) {
        badge.textContent = '0';
        container.innerHTML = '<div class="text-center py-3"><i class="bi bi-info-circle" style="font-size:24px;color:rgba(255,255,255,0.7)"></i><p class="mt-2 text-muted">' + escapeHtml('No enabled models. Add model IDs to ENABLED_MODELS in .config/config.json.') + '</p></div>';
        return;
      }
      const enabledCount = enabledModels.filter(m => !disabledModels.has(m)).length;
      badge.textContent = enabledCount;
      const preview = document.getElementById('modelPreviewText');
      if (preview) preview.textContent = enabledCount + ' of ' + enabledModels.length + ' active';
      container.innerHTML = `<div style="display:flex;flex-wrap:wrap;gap:1px">${enabledModels.map((model) => {
        const display = allDisplayNames[model] || model.replace(/^umans-/i, '');
        const isDisabled = disabledModels.has(model);
        const cls = isDisabled ? 'model-tag' : 'model-tag enabled';
        const checked = isDisabled ? '' : 'checked';
        return `<div class="${cls}" title="${escapeHtml(model)}" data-model="${escapeHtml(model)}"><input type="checkbox" ${checked}><div class="model-tag-row"><span class="model-tag-id">${escapeHtml(display)}</span></div></div>`;
      }).join('')}</div>`;
    }

    async function toggleModel(modelId) {
      if (disabledModels.has(modelId)) {
        disabledModels.delete(modelId);
      } else {
        disabledModels.add(modelId);
      }
      renderModels();
      try {
        const r = await fetch('/api/config', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ disabledModels: Array.from(disabledModels) })
        });
        const d = await r.json();
        if (!d.success) showToast('Failed:' + ' ' + (d.error||''), 'danger');
      } catch { showToast('Failed:', 'danger'); }
    }

    async function fetchUmansUserStatus() {
      try { const r=await fetch('/api/umans/user'); const d=await r.json(); umansUserId=d.user_id||umansUserId; } catch {}
    }

    async function fetchConcurrency() {
      try {
        const r = await fetch('/api/umans/concurrency?fresh=1');
        const d = await r.json();
        const concurrent = d.concurrent != null ? d.concurrent : 0;
        const limit = d.limit != null ? d.limit : 0;
        const hardCap = d.hard_cap != null ? d.hard_cap : null;
        const active = d.active != null ? d.active : concurrent;
        const queued = d.queued != null ? d.queued : 0;
        document.getElementById('concActive').textContent = active.toString();
        document.getElementById('concQueued').textContent = queued.toString();
        document.getElementById('concLimit').textContent = limit.toString();
        document.getElementById('concBurst').textContent = hardCap != null ? hardCap.toString() : '--';
        // Determine the effective gate based on concurrency limit mode.
        const mode = d.concurrency_limit_mode || _concurrencyLimitMode || 'soft';
        const manualLimit = d.manual_limit != null ? d.manual_limit : _manualLimit;
        let barMax;
        if (mode === 'manual' && manualLimit > 0) {
          barMax = manualLimit;
        } else if (mode === 'hard' && hardCap != null) {
          barMax = hardCap;
        } else {
          barMax = limit;
        }
        // Update manual slider max to current hardCap
        const slider = document.getElementById('manualLimitSlider');
        if (slider) {
          if (hardCap != null && hardCap > 0) {
            slider.max = hardCap;
            slider.disabled = false;
            const clamped = Math.min(Math.max(_manualLimit, 1), hardCap);
            slider.value = clamped;
            document.getElementById('manualLimitMax').textContent = hardCap;
          } else {
            slider.disabled = true;
            document.getElementById('manualLimitMax').textContent = '--';
          }
        }
        // Solid fill = proxy's active count, dotted overlay = upstream's concurrent count
        const activePct = barMax > 0 ? Math.min(100, (active / barMax) * 100) : 0;
        const upstreamPct = barMax > 0 ? Math.min(100, (concurrent / barMax) * 100) : 0;
        // Bottom borders: show zones when barMax > limit
        const showZones = (mode === 'hard' || (mode === 'manual' && manualLimit > limit)) && hardCap != null && limit > 0;
        const softPct = showZones ? ((limit > 0 && barMax > limit) ? (limit / barMax) * 100 : 100) : 100;
        document.getElementById('concBottomSoft').style.width = softPct + '%';
        document.getElementById('concBottomBurst').style.left = softPct + '%';
        document.getElementById('concBottomBurst').style.width = (100 - softPct) + '%';
        // Solid fill (proxy active): green in soft cap region, orange in burst region
        const bar = document.getElementById('concurrencyProgressBar');
        bar.style.width = activePct + '%';
        if (showZones && active > limit && limit > 0) {
          const limitPct = activePct > 0 ? Math.min(100, (limit / active) * 100) : 100;
          bar.style.background = `linear-gradient(90deg, #34d399 0%, #34d399 ${limitPct}%, #fb923c ${limitPct}%, #fb923c 100%)`;
          bar.style.boxShadow = '0 0 8px rgba(251,146,60,0.4)';
        } else {
          bar.style.background = '#34d399';
          bar.style.boxShadow = '0 0 8px rgba(52,211,153,0.4)';
        }
        // Dotted overlay (upstream concurrent)
        document.getElementById('concurrencyUpstreamBar').style.width = upstreamPct + '%';
        document.getElementById('concUsageText').textContent = active + ' of ' + barMax + ' sessions';
        const pctVsSoft = barMax > 0 ? Math.min(200, (active / barMax) * 100) : 0;
        document.getElementById('concPctText').textContent = parseFloat(pctVsSoft.toFixed(2)) + '%';
        const pb = document.querySelector('[role="progressbar"]');
        if (pb) pb.setAttribute('aria-valuenow', Math.round(activePct).toString());
        // Detail items
        const detailGrid = document.getElementById('concurrencyDetailGrid');
        const details = [];
        if (d.user_id) {
          umansUserId = d.user_id;
          const topUid = document.getElementById('topUserId');
          if (topUid) {
            topUid.dataset.userid = d.user_id;
            if (!topUid.classList.contains('revealed')) {
              topUid.textContent = '•'.repeat(Math.min(d.user_id.length, 12));
            }
          }
        }
        detailGrid.innerHTML = details.map(d =>
          `<div class="${d.color?`border-top-color:${d.color};`:''}"><div class="card stat-inline-card h-100"><div class="card-content" style="${d.color?`border-top-color:${d.color};`:''}"><div class="stats-value" style="color:${d.warn?'#fbbf24':'#fff'}${d.masked?'':';font-variant-numeric:tabular-nums'}">${d.masked?`<span class="masked-userid" data-userid="${escapeHtml(d.value)}" onclick="this.classList.toggle('revealed');this.textContent=this.classList.contains('revealed')?this.dataset.userid:'${'•'.repeat(Math.min(d.value.length, 12))}'" style="cursor:pointer">${'•'.repeat(Math.min(d.value.length, 12))}</span>`:escapeHtml(d.value)}</div><div class="stats-label">${escapeHtml(d.label)}</div></div></div></div>`
        ).join('');
        layoutStatGrids();
      } catch {
        // Leave defaults (--)
      }
    }

    async function openKeysModal() {
      try {
        await Promise.all([fetchUmansUserStatus(), fetchConcurrency()]);
        const kr=await fetch('/api/keys'),kd=await kr.json(); editingIdx=-1; renderKeysList(kd.keys||[]);
        new bootstrap.Modal(document.getElementById('keysModal')).show();
      } catch(e) { showToast('Failed to load keys','danger'); }
    }
    function copyUserId() {
      if (umansUserId) {
        navigator.clipboard.writeText(umansUserId).then(() => showToast('User ID copied','success')).catch(() => showToast('Copy failed','danger'));
      }
    }
    function renderKeysList(keys) {
      const container = document.getElementById('keysList');
      const real=keys.filter(k=>k.key);
      const displayUserId = umansUserId;
      const userIdRow = umansUserId ? `<div class="mt-1 d-flex align-items-center gap-2" style="font-size:0.7rem;color:rgba(255,255,255,0.55)">
          <span><i class="bi bi-person-badge me-1"></i>${escapeHtml('User ID')}:</span>
          <code class="masked-userid" data-userid="${escapeHtml(displayUserId)}" onclick="this.classList.toggle('revealed');this.textContent=this.classList.contains('revealed')?this.dataset.userid:'${'•'.repeat(Math.min(displayUserId.length, 12))}'" style="font-size:0.7rem;color:rgba(255,255,255,0.85);background:rgba(255,255,255,0.05);padding:1px 4px;border-radius:4px;word-break:break-all;cursor:pointer">${'•'.repeat(Math.min(displayUserId.length, 12))}</code>
          <button class="btn btn-sm p-0 ms-auto" style="background:transparent;border:none;color:rgba(255,255,255,0.75);font-size:0.65rem" onclick="copyUserId()" title="${escapeHtml('User ID copied')}"><i class="bi bi-clipboard"></i></button>
        </div>` : '';
      const accountHtml = `<div style="font-size:10px;color:rgba(255,255,255,0.75);text-transform:uppercase;letter-spacing:0.8px;margin-bottom:4px">${escapeHtml('Account')}:</div>
        <div class="mb-2 p-2" style="background:rgba(255,255,255,0.04);border-radius:8px;border:1px solid rgba(255,255,255,0.1);font-size:0.8rem">
          <div class="d-flex justify-content-between align-items-center">
            <span style="color:rgba(255,255,255,0.7)"><i class="bi bi-person-circle me-1"></i>API key</span>
          </div>
          ${userIdRow}
        </div>`;
      let keysHtml = '';
      if (real.length) {
        keysHtml = '<div class="mt-2" style="font-size:10px;color:rgba(255,255,255,0.75);text-transform:uppercase;letter-spacing:0.8px;margin-bottom:4px">' + escapeHtml('Keys') + ':</div>' +
        real.map((k,i)=>{
          const oi = keys.indexOf(k);
          if(editingIdx===oi) return `<div class="pool-card"><div class="mb-2"><label class="form-label" style="font-size:10px">${escapeHtml('Name')}</label><input type="text" class="form-control form-control-sm" id="editKeyName" value="${escapeHtml(k.name||'')}"></div><div class="mb-2"><label class="form-label" style="font-size:10px">${escapeHtml('Key')}</label><input type="text" class="form-control form-control-sm" id="editKey" value="${escapeHtml(k.key||'')}"></div><div class="d-flex gap-2"><button class="btn btn-sm btn-success" onclick="saveEditKey(${oi})"><i class="bi bi-check me-1"></i>${escapeHtml('Save')}</button><button class="btn btn-sm btn-secondary" onclick="cancelEditKey()">${escapeHtml('Cancel')}</button></div></div>`;
          const mk = escapeHtml(k.key.substring(0,4)+'...'+k.key.substring(k.key.length-4));
          return `<div class="pool-card"><div class="pool-header"><div class="pool-name">${escapeHtml(k.name||'Unnamed')}</div><div><button class="btn btn-sm btn-outline-action me-1" onclick="startEditKey(${oi})" title="${escapeHtml('Edit')}"><i class="bi bi-pencil"></i></button><button class="btn btn-sm" style="background:rgba(248,113,113,0.2);color:#f87171;border:none" onclick="deleteKey(${oi})" title="${escapeHtml('Delete')}"><i class="bi bi-trash"></i></button></div></div><div style="font-size:0.8rem;color:rgba(255,255,255,0.7)"><span title="${escapeHtml(k.key)}">${mk}</span></div></div>`;
        }).join('');
      } else { keysHtml = '<p class="text-muted text-center py-3">' + escapeHtml('No API keys configured.') + '</p>'; }
      container.innerHTML = accountHtml + keysHtml;
    }
    function refreshKeysList() { return fetch('/api/keys').then(r=>r.json()).then(d=>renderKeysList(d.keys||[])).catch(()=>{}); }
    function startEditKey(i) { editingIdx=i; refreshKeysList(); }
    function cancelEditKey() { editingIdx=-1; refreshKeysList(); }
    async function saveEditKey(i) { const n=document.getElementById('editKeyName').value.trim(),k=document.getElementById('editKey').value.trim(); try{const r=await fetch('/api/keys',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({action:'update',index:i,name:n,key:k})});const d=await r.json();if(d.success){editingIdx=-1;showToast('Key updated');loadConfig();}else showToast('Failed:' + ' ' + d.error,'danger');}catch{showToast('Failed:','danger');} }
    async function deleteKey(i) { try{const r=await fetch('/api/keys',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({action:'delete',index:i})});const d=await r.json();if(d.success){showToast('Key deleted');loadConfig();}else showToast('Failed:' + ' ' + d.error,'danger');}catch{showToast('Failed:','danger');} }
    async function addKey() { const n=document.getElementById('newKeyName').value.trim()||('Key' + ' '+(document.querySelectorAll('#keysList .pool-card').length+1)),k=document.getElementById('newKey').value.trim(); if(!k){showToast('Key required','warning');return;} try{const r=await fetch('/api/keys',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({action:'add',name:n,key:k})});const d=await r.json();if(d.success){document.getElementById('newKeyName').value='';document.getElementById('newKey').value='';showToast('Key added');loadConfig();}else showToast('Failed:' + ' ' + d.error,'danger');}catch{showToast('Failed:','danger');} }

    async function loadConfig() {
      try {
        const [cr,mr]=await Promise.all([
          fetch('/api/config'), fetch('/api/models')
        ]);
        // healthz is fetched in the background — it hits upstream and can be slow.
        // Don't block the dashboard load on it.
        fetch('/healthz').then(r=>r.json()).then(d=>{ proxyData=d; renderTokenPools(); updateStats(); }).catch(()=>{});
        const cd=await cr.json(); const md=await mr.json();
        enabledModels = md.models||[];
        disabledModels = new Set(md.disabled_models || []);
        allDisplayNames = md.model_display_names||{};
        if(cd) {
          if (cd.wallpaperSource && cd.wallpaperSource !== 'none') {
            setWallpaperActive(cd.wallpaperSource);
            const computedBg = getComputedStyle(document.body).backgroundImage;
            if (!computedBg || computedBg === 'none') {
              await setWallpaper(cd.wallpaperSource, true);
            } else {
              document.body.classList.add('has-wallpaper');
            }
          } else {
            setWallpaperActive('none');
            clearWallpaper();
          }
          // Vision handoff + cache init
          setVisionHandoffActive(!!cd.visionHandoffEnabled);
          setHandoffCacheActive(cd.visionHandoffCacheEnabled !== false);
          const cacheWrapper = document.getElementById('handoffCacheWrapper');
          if (cacheWrapper) cacheWrapper.style.display = cd.visionHandoffEnabled ? '' : 'none';
          // Concurrency limit mode init (from backend config)
          const concMode = cd.concurrencyLimitMode || 'soft';
          // Set _manualLimit BEFORE setConcurrencyLimitActive so the slider
          // position is correct on first render.
          if (cd.manualConcurrencyLimit != null && cd.manualConcurrencyLimit > 0) {
            _manualLimit = cd.manualConcurrencyLimit;
          }
          setConcurrencyLimitActive(concMode);
          // API key mode init (smart/managed/passthrough)
          setApiKeyModeActive(cd.apikeyMode || 'smart');
          // Slot free delay init
          const sfd = cd.slotFreeDelay != null ? cd.slotFreeDelay : 2;
          const sfdSlider = document.getElementById('slotFreeDelaySlider');
          if (sfdSlider) sfdSlider.value = sfd;
          const sfdVal = document.getElementById('slotFreeDelayValue');
          if (sfdVal) sfdVal.textContent = sfd + 's';
          // Retry attempts init
          const retryAttempts = cd.retryAttempts != null ? cd.retryAttempts : DEFAULT_RETRY_ATTEMPTS;
          const raSlider = document.getElementById('retryAttemptsSlider');
          if (raSlider) { raSlider.value = retryAttempts; raSlider.dataset.prev = retryAttempts; }
          const raVal = document.getElementById('retryAttemptsValue');
          if (raVal) raVal.textContent = retryAttempts;
          updateBackoffVisibility(retryAttempts);
          // Backoff strategy init
          const bsSelect = document.getElementById('backoffStrategySelect');
          if (bsSelect) {
            bsSelect.value = cd.backoffStrategy || 'aggressive';
            bsSelect.dataset.prev = cd.backoffStrategy || 'aggressive';
          }
          updateBackoffPreview(retryAttempts, cd.backoffStrategy || 'aggressive');
          // Request timeout init
          const timeoutSecs = cd.requestTimeout != null ? cd.requestTimeout : DEFAULT_REQUEST_TIMEOUT;
          const rtSlider = document.getElementById('requestTimeoutSlider');
          if (rtSlider) { rtSlider.value = timeoutSecs; rtSlider.dataset.prev = timeoutSecs; }
          const rtVal = document.getElementById('requestTimeoutValue');
          if (rtVal) rtVal.textContent = formatSeconds(timeoutSecs);
        }
        // Mobile: collapse right-column sections by default
        if (window.matchMedia('(max-width: 575px)').matches) {
          ['quick-settings', 'actions', 'tokens', 'models', 'env'].forEach(s => {
            const sec = document.getElementById('section-' + s);
            const icon = document.querySelector('[data-section="' + s + '"] .toggle-icon');
            const hdr = document.querySelector('[data-section="' + s + '"] .collapsible-header');
            if (sec && !sec.classList.contains('collapsed')) {
              sec.classList.add('collapsed');
              sec.style.display = 'none'; // hide fully — CSS .collapsed only zeros height/padding
              if (icon) { icon.classList.add('collapsed'); icon.classList.remove('bi-chevron-down'); icon.classList.add('bi-chevron-right'); }
              if (hdr) hdr.setAttribute('aria-expanded', 'false');
            }
          });
        }
        renderModels(); renderTokenPools(); updateStats();
        // Hide loader now — config, models, and wallpaper are ready.
        // fetchUsage is fire-and-forget; it populates the 5-Hour Window card
        // but doesn't need to block the dashboard from showing.
        const loader = document.getElementById('wallpaperLoader');
        if (loader) loader.style.display = 'none';
        fetchUsage();
        setTimeout(initLiquidGlass, 50);
        setTimeout(()=>window.dispatchEvent(new Event('resize')),500);
      } catch(e) { showToast('Failed to load configuration','danger'); }
    }

    async function fetchUsage() {
      try {
        const ur=await fetch('/api/umans/usage?fresh=1');
        const u=await ur.json();
        const usage = u.usage || {};
        const win = u.window || {};
        const plan = u.plan || {};
        const winReqs = usage.requests_in_window || 0;
        const tokensIn = usage.tokens_in || 0;
        const tokensOut = usage.tokens_out || 0;
        const cached = usage.tokens_cached || 0;
        const cachedPct = tokensIn > 0 ? ((cached/tokensIn)*100).toFixed(1) : '0.0';
        document.getElementById('totalRequests').textContent = winReqs.toLocaleString();
        document.getElementById('cachedPct').textContent = cachedPct + '%';
        // Priority card (always visible, replaces old Throttled card)
        const prioEl = document.getElementById('priorityStatus');
        if (prioEl) {
          if (usage.priority && usage.priority.low) {
            prioEl.textContent = 'Low' + (usage.priority.reason ? ' (' + usage.priority.reason + ')' : '');
            prioEl.style.color = '#f87171';
          } else {
            prioEl.textContent = 'Normal';
            prioEl.style.color = '#fff';
          }
        }
        if (plan.display_name) {
          document.getElementById('planBadge').textContent = plan.display_name;
          document.getElementById('planBadge').style.display = '';
        } else {
          document.getElementById('planBadge').style.display = 'none';
        }
        const statGrid = document.getElementById('usageStatGrid');
        // Remove previously appended detail cards (keep first 3 static)
        while (statGrid.children.length > 3) {
          statGrid.removeChild(statGrid.lastChild);
        }
        const details = [];
        const startTime = win.started_at ? new Date(win.started_at).toLocaleTimeString() : '--';
        details.push({label:'Start Time', value: startTime, color:'#f472b6'});
        details.push({label:'Tokens In', value: formatCompact(tokensIn), color:'#fbbf24'});
        details.push({label:'Tokens Out', value: formatCompact(tokensOut), color:'#fb923c'});
        // Priority is no longer appended dynamically — it's a static card now
        details.forEach(d => {
          const div = document.createElement('div');
          div.innerHTML = `<div class="card stat-inline-card h-100"><div class="card-content" style="${d.color?`border-top-color:${d.color};`:''}"><div class="stats-value" style="color:${d.warn?'#fbbf24':'#fff'};font-size:0.75rem">${escapeHtml(d.value)}</div><div class="stats-label">${escapeHtml(d.label)}</div></div></div>`;
          statGrid.appendChild(div);
        });
        layoutStatGrids();
      } catch { document.getElementById('priorityStatus').textContent='--';document.getElementById('totalRequests').textContent='--';document.getElementById('cachedPct').textContent='--'; }
    }

    function updateStats() {
      if(!proxyData) return;
      const ver = proxyData.version || '-';
      const verEl = document.getElementById('versionInfo');
      if (verEl) verEl.textContent = ver;
      const rt = (proxyData.runtime||'node')+' v'+(proxyData.runtime_version||'');
      document.getElementById('runtimeInfo').textContent = rt;
      const envPrev = document.getElementById('envPreviewText');
      if (envPrev) envPrev.textContent = 'v' + ver + ' · ' + rt + ' · :' + (proxyData.port||'8084');
      document.getElementById('portInfo').textContent = proxyData.port||'8084';
      const started = proxyData.started_at ? new Date(proxyData.started_at).toLocaleString() : '-';
      document.getElementById('startedAt').textContent = started;
      // Handoff cache stats
      const vh = proxyData.visionHandoff;
      const hcs = document.getElementById('handoffCacheStats');
      if (hcs && vh && vh.cache) {
        const c = vh.cache;
        hcs.textContent = c.hits + ' hits, ' + c.misses + ' misses (' + c.size + ' entries)';
      }
    }

    let usageIntervalId = null;
    let historyIntervalId = null;
    let historyState = { range: '7d', granularity: 'day', metric: 'tokens', hiddenStatuses: new Set(), loading: false, error: null, selectedBucket: null, totalRows: 0, detailSort: { key: null, dir: -1 }, tableSort: { key: 'bucket', dir: -1 } };
    const HISTORY_STATUS_COLORS = {
      ok: { color: '#34d399', label: 'OK' },
      client_cancelled: { color: '#fbbf24', label: 'Cancelled' },
      upstream_error: { color: '#fb923c', label: 'Upstream Error' },
      rate_limited: { color: '#f87171', label: 'Rate Limited' },
      auth_error: { color: '#94a3b8', label: 'Auth Error' },
      gateway_error: { color: '#22d3ee', label: 'Gateway Error' },
    };
    const ERROR_STATUSES = new Set(['upstream_error', 'rate_limited', 'auth_error', 'gateway_error']);
    function formatCompact(n) {
      if (n == null || isNaN(n)) return '0';
      const abs = Math.abs(n);
      if (abs >= 1e9) return (n / 1e9).toFixed(1) + 'B';
      if (abs >= 1e6) return (n / 1e6).toFixed(1) + 'M';
      if (abs >= 1e3) return (n / 1e3).toFixed(1) + 'K';
      return n.toLocaleString(undefined, { maximumFractionDigits: 1 });
    }
    function getHistoryRange(range) {
      const to = new Date();
      const from = new Date();
      if (range === '24h') from.setHours(from.getHours() - 24);
      else if (range === '7d') from.setDate(from.getDate() - 7);
      else if (range === '30d') from.setDate(from.getDate() - 30);
      return { from: from.toISOString(), to: to.toISOString() };
    }
    function setHistoryRange(range) {
      historyState.range = range;
      historyState.selectedBucket = null;
      document.querySelectorAll('.btn-hist-range').forEach(btn => btn.classList.toggle('active', btn.dataset.range === range));
      const wantsHour = range === '24h';
      if (wantsHour && historyState.granularity !== 'hour') {
        historyState.granularity = 'hour';
        document.querySelectorAll('.btn-hist-gran').forEach(btn => btn.classList.toggle('active', btn.dataset.gran === 'hour'));
      }
      fetchHistory();
    }
    function setHistoryGranularity(gran) {
      historyState.granularity = gran;
      historyState.selectedBucket = null;
      document.querySelectorAll('.btn-hist-gran').forEach(btn => btn.classList.toggle('active', btn.dataset.gran === gran));
      fetchHistory();
    }
    function setHistoryMetric(metric) {
      historyState.metric = metric;
      historyState.detailSort = { key: null, dir: -1 };
      document.querySelectorAll('.btn-hist-metric').forEach(btn => btn.classList.toggle('active', btn.dataset.metric === metric));
      const legendWrapper = document.getElementById('historyLegendWrapper');
      if (metric === 'tokens') {
        // Keep only OK; hide all other statuses
        historyState.hiddenStatuses = new Set(['client_cancelled', 'upstream_error', 'rate_limited', 'auth_error', 'gateway_error']);
        if (legendWrapper) legendWrapper.style.display = 'none';
      } else {
        // Requests: clear forced hides, let user control via legend
        historyState.hiddenStatuses = new Set();
        if (legendWrapper) legendWrapper.style.display = '';
      }
      renderHistory();
    }
    function toggleHistoryStatus(status) {
      if (historyState.hiddenStatuses.has(status)) historyState.hiddenStatuses.delete(status);
      else historyState.hiddenStatuses.add(status);
      renderHistory();
    }
    let lastHistoryData = null;
    // Client-side cache of upstream history responses, keyed by granularity.
    // Always fetch 30d so 7d, 30d, and 24h are all subsets of one fetch.
    const historyCache = {}; // { [granularity]: { data, fetchedAt } }
    const HISTORY_CACHE_TTL = 5 * 60 * 1000; // 5 minutes, matches server-side UsageCacheTTL

    async function fetchHistory() {
      const chartEl = document.getElementById('historyChart');
      historyState.loading = true;
      historyState.error = null;
      if (chartEl && !lastHistoryData) {
        chartEl.innerHTML = '<div class="w-100 text-center text-muted" style="font-size:0.75rem;display:flex;align-items:center;justify-content:center;height:100%;gap:6px"><span class="spinner-border spinner-border-sm" role="status"></span>Loading...</div>';
      }
      const { from, to } = getHistoryRange(historyState.range);
      const gran = historyState.granularity;

      const cached = historyCache[gran];
      if (cached && (Date.now() - cached.fetchedAt) < HISTORY_CACHE_TTL) {
        const fromMs = new Date(from).getTime();
        const toMs = new Date(to).getTime();
        const filtered = (cached.data.buckets || []).filter(b => {
          const bMs = new Date(b.bucket || '').getTime();
          return bMs >= fromMs && bMs <= toMs;
        });
        lastHistoryData = { ...cached.data, buckets: filtered };
        historyState.loading = false;
        historyState.error = null;
        renderHistory();
        return;
      }

      // Cache miss — always fetch 30d so the cache covers all ranges.
      const f = new Date(); f.setDate(f.getDate() - 30);
      const fetchFrom = f.toISOString();
      const fetchTo = new Date().toISOString();
      try {
        const r = await fetch(`/api/umans/usage-history?from=${encodeURIComponent(fetchFrom)}&to=${encodeURIComponent(fetchTo)}&granularity=${gran}`);
        if (!r.ok) throw new Error(`HTTP ${r.status}`);
        const d = await r.json();
        historyCache[gran] = { data: d, fetchedAt: Date.now() };
        // Filter to the originally requested range
        const fromMs = new Date(from).getTime();
        const toMs = new Date(to).getTime();
        const filtered = (d.buckets || []).filter(b => {
          const bMs = new Date(b.bucket || '').getTime();
          return bMs >= fromMs && bMs <= toMs;
        });
        lastHistoryData = { ...d, buckets: filtered };
        historyState.loading = false;
        historyState.error = null;
        renderHistory();
      } catch (e) {
        historyState.loading = false;
        historyState.error = e.message || 'Failed to load history';
        renderHistory();
      }
    }
    function groupHistoryBuckets(buckets) {
      const groups = {};
      for (const b of (buckets || [])) {
        const key = b.bucket || '';
        if (!groups[key]) groups[key] = { bucket: key, rows: [] };
        groups[key].rows.push(b);
      }
      return Object.values(groups).sort((a, b) => new Date(a.bucket) - new Date(b.bucket));
    }
    function aggregateHistoryTotals(buckets) {
      let requests = 0, weighted = 0, tokensIn = 0, tokensOut = 0, cachedRead = 0, errorReqs = 0, peakConc = 0, peakWeighted = 0;
      let allRequests = 0, allErrorReqs = 0;
      for (const b of (buckets || [])) {
        const isErr = ERROR_STATUSES.has(getHistoryStatus(b));
        // Always count errors from all buckets for the stat card
        allRequests += b.requests || 0;
        if (isErr) allErrorReqs += b.requests || 0;
        if (historyState.hiddenStatuses.has(getHistoryStatus(b))) continue;
        requests += b.requests || 0;
        weighted += b.weighted_requests || 0;
        tokensIn += b.tokens_in_total || b.tokens_in || 0;
        tokensOut += b.tokens_out || 0;
        cachedRead += b.tokens_cached_read || 0;
        const peak = b.account_peak_concurrent_requests || b.peak_concurrent_requests || 0;
        if (peak > peakConc) peakConc = peak;
        const peakW = b.account_peak_weighted_concurrent_requests || b.peak_weighted_concurrent_requests || 0;
        if (peakW > peakWeighted) peakWeighted = peakW;
        if (isErr) errorReqs += b.requests || 0;
      }
      const cachedPct = tokensIn > 0 ? (cachedRead / tokensIn) * 100 : 0;
      // In Tokens mode, error rate reflects all buckets (status hiding is a metric constraint, not a filter).
      // In Requests mode, error rate reflects only visible (unhidden) statuses.
      const errorRate = historyState.metric === 'tokens'
        ? (allRequests > 0 ? (allErrorReqs / allRequests) * 100 : 0)
        : (requests > 0 ? (errorReqs / requests) * 100 : 0);
      return { requests, weighted, tokensIn, tokensOut, cachedRead, cachedPct, errorReqs, errorRate, peakConc, peakWeighted };
    }
    function getHistoryStatus(b) {
      return b.status || b.error_category || 'ok';
    }
    function renderHistoryLegend(statuses) {
      const container = document.getElementById('historyLegend');
      if (!container) return;
      container.innerHTML = statuses.map(s => {
        const info = HISTORY_STATUS_COLORS[s] || { color: '#94a3b8', label: s };
        const hidden = historyState.hiddenStatuses.has(s);
        return `<button class="btn btn-sm" onclick="toggleHistoryStatus('${s}')" style="background:${hidden ? 'transparent' : info.color + '20'};border:1px solid ${info.color};color:${hidden ? 'rgba(255,255,255,0.5)' : '#fff'};padding:1px 6px;font-size:0.6rem;border-radius:4px;display:inline-flex;align-items:center;gap:4px"><span style="width:8px;height:8px;border-radius:50%;background:${hidden ? 'rgba(255,255,255,0.3)' : info.color};display:inline-block"></span>${escapeHtml(info.label)}</button>`;
      }).join('');
    }
    function renderHistoryTooltipContent(g) {
      const metric = historyState.metric;
      const rows = g.rows.slice().sort((a, b) => (b.requests || 0) - (a.requests || 0));
      const total = rows.reduce((sum, r) => sum + (metric === 'tokens' ? ((r.tokens_in_total || r.tokens_in || 0) + (r.tokens_out || 0)) : (r.requests || 0)), 0);
      const dateLabel = historyState.granularity === 'day'
        ? new Date(g.bucket).toLocaleDateString(undefined, { weekday: 'short', year: 'numeric', month: 'short', day: 'numeric' })
        : new Date(g.bucket).toLocaleString(undefined, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' });
      const statusSummary = {};
      for (const r of rows) {
        const st = getHistoryStatus(r);
        if (!statusSummary[st]) statusSummary[st] = { requests: 0, weighted: 0, tokensIn: 0, tokensOut: 0, count: 0 };
        statusSummary[st].requests += r.requests || 0;
        statusSummary[st].weighted += r.weighted_requests || 0;
        statusSummary[st].tokensIn += r.tokens_in_total || r.tokens_in || 0;
        statusSummary[st].tokensOut += r.tokens_out || 0;
        statusSummary[st].count += 1;
      }
      const statusRows = metric === 'tokens' ? '' : Object.entries(statusSummary).sort((a, b) => b[1].requests - a[1].requests).map(([st, agg]) => {
        const info = HISTORY_STATUS_COLORS[st] || { color: '#94a3b8', label: st };
        const val = formatCompact(agg.requests);
        return `<div style="display:flex;justify-content:space-between;align-items:center;gap:8px;margin-top:3px"><span style="display:flex;align-items:center;gap:4px"><span style="width:8px;height:8px;border-radius:50%;background:${info.color}"></span><span style="color:${info.color}">${escapeHtml(info.label)}</span></span><span style="font-variant-numeric:tabular-nums">${val}</span></div>`;
      }).join('');
      const topRows = (() => {
        const byModel = {};
        for (const r of rows) {
          const name = r.provider || r.model || 'unknown';
          if (!byModel[name]) byModel[name] = { requests: 0, tokensIn: 0, tokensOut: 0 };
          byModel[name].requests += r.requests || 0;
          byModel[name].tokensIn += r.tokens_in_total || r.tokens_in || 0;
          byModel[name].tokensOut += r.tokens_out || 0;
        }
        return Object.entries(byModel)
          .map(([name, agg]) => ({
            name,
            val: metric === 'tokens' ? (agg.tokensIn + agg.tokensOut) : agg.requests,
          }))
          .filter(e => e.val > 0)
          .sort((a, b) => b.val - a.val)
          .slice(0, 3)
          .map(({ name, val }) =>
            `<div style="display:flex;justify-content:space-between;gap:6px;margin-top:2px;color:rgba(255,255,255,1)"><span>${escapeHtml(name)}</span><span style="font-variant-numeric:tabular-nums">${formatCompact(val)}</span></div>`
          ).join('');
      })();
      return `<div style="font-weight:600;margin-bottom:4px;border-bottom:1px solid rgba(255,255,255,0.15);padding-bottom:3px">${escapeHtml(dateLabel)}</div>
        <div style="color:rgba(255,255,255,0.85);margin-bottom:4px">Total: <span style="color:#fff;font-weight:600">${metric === 'tokens' ? formatCompact(total) + ' tokens' : formatCompact(total) + ' requests'}</span></div>
        ${statusRows}
        <div style="margin-top:6px;border-top:1px solid rgba(255,255,255,0.12);padding-top:4px;color:rgba(255,255,255,0.85)">Top models</div>${topRows}`;
    }
    let _historyDocClickHandler = null;
    function renderHistoryChart(groups) {
      const container = document.getElementById('historyChart');
      const yAxisEl = document.getElementById('historyYAxis');
      const xAxisEl = document.getElementById('historyXAxis');
      if (historyState.error) {
        container.innerHTML = '<div class="w-100 text-center" style="font-size:0.75rem;color:#f87171;display:flex;align-items:center;justify-content:center;height:100%;gap:4px"><i class="bi bi-exclamation-triangle"></i>Failed to load history</div>';
        if (yAxisEl) yAxisEl.innerHTML = '';
        if (xAxisEl) xAxisEl.innerHTML = '';
        return;
      }
      if (!groups.length) {
        container.innerHTML = '<div class="w-100 text-center text-muted" style="font-size:0.75rem;display:flex;align-items:center;justify-content:center;height:100%">No history data</div>';
        if (yAxisEl) yAxisEl.innerHTML = '';
        if (xAxisEl) xAxisEl.innerHTML = '';
        return;
      }
      container.replaceChildren();
      const metric = historyState.metric;
      let maxVal = 0;
      for (const g of groups) {
        let val = 0;
        for (const r of g.rows) {
          const status = getHistoryStatus(r);
          if (historyState.hiddenStatuses.has(status)) continue;
          val += metric === 'tokens' ? ((r.tokens_in_total || r.tokens_in || 0) + (r.tokens_out || 0)) : (r.requests || 0);
        }
        if (val > maxVal) maxVal = val;
      }
      if (maxVal <= 0) maxVal = 1;
      // Build Y-axis labels and grid lines (5 ticks)
      const yTicks = 5;
      if (yAxisEl) {
        yAxisEl.innerHTML = '';
        for (let i = yTicks; i >= 0; i--) {
          const v = (maxVal * i / yTicks);
          const label = document.createElement('div');
          label.textContent = formatCompact(v);
          label.style.lineHeight = '1';
          yAxisEl.appendChild(label);
        }
      }
      // Add horizontal grid lines
      container.style.background = 'rgba(0,0,0,0.2)';
      const gridOverlay = document.createElement('div');
      gridOverlay.style.cssText = 'position:absolute;inset:0;pointer-events:none;z-index:0';
      for (let i = 1; i < yTicks; i++) {
        const line = document.createElement('div');
        line.style.cssText = `position:absolute;left:0;right:0;top:${(i / yTicks * 100).toFixed(2)}%;border-top:1px dashed rgba(255,255,255,0.06)`;
        gridOverlay.appendChild(line);
      }
      container.appendChild(gridOverlay);
      const statuses = new Set(groups.flatMap(g => g.rows.map(r => getHistoryStatus(r))));
      const statusList = Array.from(statuses).sort();
      const statusOrder = ['ok', 'client_cancelled', 'upstream_error', 'rate_limited', 'auth_error', 'gateway_error'];
      const orderedStatuses = statusOrder.filter(s => statusList.includes(s)).concat(statusList.filter(s => !statusOrder.includes(s)));
      const isDay = new Date(groups[0].bucket).toISOString().slice(11, 13) === '00' && historyState.granularity === 'day';
      const labelInterval = Math.max(1, Math.ceil(groups.length / 8));
      const tooltip = document.getElementById('historyTooltip');
      // Build X-axis labels
      if (xAxisEl) { xAxisEl.innerHTML = ''; }
      groups.forEach((g, idx) => {
        const segments = orderedStatuses.map(s => {
          let val = 0;
          for (const r of g.rows) { if (getHistoryStatus(r) === s && !historyState.hiddenStatuses.has(s)) val += metric === 'tokens' ? ((r.tokens_in_total || r.tokens_in || 0) + (r.tokens_out || 0)) : (r.requests || 0); }
          return { status: s, val };
        }).filter(seg => seg.val > 0);
        const total = segments.reduce((a, s) => a + s.val, 0);
        const pct = Math.max(1.5, Math.round((total / maxVal) * 100));
        const unitPct = total > 0 ? 100 / total : 0;
        const showLabel = idx % labelInterval === 0 || idx === groups.length - 1;
        const label = isDay
          ? new Date(g.bucket).toLocaleDateString(undefined, { month: 'short', day: 'numeric' })
          : new Date(g.bucket).toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' });
        const col = document.createElement('div');
        col.className = 'history-bar-col';
        col.dataset.bucket = g.bucket;
        col.style.cssText = 'flex:1;min-width:8px;display:flex;flex-direction:column;align-items:center;justify-content:flex-end;height:100%;gap:0;position:relative;transition:opacity 0.15s ease;z-index:1';
        if (historyState.selectedBucket && historyState.selectedBucket !== g.bucket) col.style.opacity = '0.45';
        col.innerHTML = `<div class="history-bar-stack" style="width:85%;max-width:36px;display:flex;flex-direction:column;justify-content:flex-end;height:0%;border-radius:3px 3px 0 0;overflow:hidden;transition:height 0.5s cubic-bezier(0.22,1,0.36,1),filter 0.15s ease;box-shadow:0 2px 8px rgba(0,0,0,0.3), inset 0 1px 0 rgba(255,255,255,0.08)">
            ${segments.map(seg => `<div class="history-seg" data-status="${seg.status}" style="width:100%;height:${(seg.val * unitPct).toFixed(4)}%;background:${(HISTORY_STATUS_COLORS[seg.status] || { color: '#94a3b8' }).color};min-height:1px;transition:transform 0.15s ease"></div>`).join('')}
          </div>`;
        const stack = col.querySelector('.history-bar-stack');
        requestAnimationFrame(() => { stack.style.height = pct + '%'; });
        col.addEventListener('click', (e) => {
          e.stopPropagation();
          if (historyState.selectedBucket === g.bucket) {
            historyState.selectedBucket = null;
            tooltip.style.display = 'none';
            container.querySelectorAll('.history-bar-col').forEach(c => { c.style.opacity = ''; });
            renderHistoryTable(groups);
          } else {
            historyState.selectedBucket = g.bucket;
            container.querySelectorAll('.history-bar-col').forEach(c => { c.style.opacity = c.dataset.bucket === g.bucket ? '' : '0.45'; });
            renderHistoryTable(groups.filter(x => x.bucket === g.bucket));
            // Position tooltip near the click, clamped to viewport
            tooltip.innerHTML = renderHistoryTooltipContent(g);
            tooltip.style.display = 'block';
            const tw = tooltip.offsetWidth;
            const th = tooltip.offsetHeight;
            let left = e.clientX - (tw / 2);
            left = Math.max(8, Math.min(left, window.innerWidth - tw - 8));
            let top = e.clientY - th - 14;
            if (top < 8) top = e.clientY + 14;
            tooltip.style.left = left + 'px';
            tooltip.style.top = top + 'px';
          }
        });
        container.appendChild(col);
        // X-axis label
        if (xAxisEl) {
          const xLabel = document.createElement('div');
          xLabel.style.cssText = `flex:1;min-width:8px;text-align:center;font-size:0.5rem;color:rgba(255,255,255,0.5);white-space:nowrap;overflow:visible`;
          xLabel.textContent = showLabel ? label : '';
          xAxisEl.appendChild(xLabel);
        }
      });
      if (_historyDocClickHandler) document.removeEventListener('click', _historyDocClickHandler);
      _historyDocClickHandler = (e) => {
        if (!e.target.closest('#historyChart') && !e.target.closest('#historyTableContainer')) {
          tooltip.style.display = 'none';
          if (historyState.selectedBucket) {
            historyState.selectedBucket = null;
            container.querySelectorAll('.history-bar-col').forEach(c => { c.style.opacity = ''; });
            renderHistoryTable(groups);
          }
        }
      };
      document.addEventListener('click', _historyDocClickHandler);
      renderHistoryLegend(orderedStatuses);
    }
    function renderHistoryTable(groups) {
      const container = document.getElementById('historyTableContainer');
      const footer = document.getElementById('historyRowCount');
      if (!groups.length) { container.innerHTML = ''; if (footer) footer.textContent = ''; return; }
      // Aggregate each group (date) into a single consolidated row
      const consolidated = groups.map(g => {
        const rows = g.rows.filter(r => !historyState.hiddenStatuses.has(getHistoryStatus(r)));
        let requests = 0, weighted = 0, tokensIn = 0, tokensOut = 0, cachedRead = 0, peak = 0, peakWeighted = 0;
        for (const r of rows) {
          requests += r.requests || 0;
          weighted += r.weighted_requests || 0;
          tokensIn += r.tokens_in_total || r.tokens_in || 0;
          tokensOut += r.tokens_out || 0;
          cachedRead += r.tokens_cached_read || 0;
          const p = r.account_peak_concurrent_requests || r.peak_concurrent_requests || 0;
          if (p > peak) peak = p;
          const pw = r.account_peak_weighted_concurrent_requests || r.peak_weighted_concurrent_requests || 0;
          if (pw > peakWeighted) peakWeighted = pw;
        }
        return { bucket: g.bucket, rows, requests, weighted, tokensIn, tokensOut, cachedRead, peak, peakWeighted };
      });
      // Sort consolidated rows by tableSort state
      const tsKey = historyState.tableSort.key;
      const tsDir = historyState.tableSort.dir;
      consolidated.sort((a, b) => {
        let va, vb;
        if (tsKey === 'bucket') { va = new Date(a.bucket).getTime(); vb = new Date(b.bucket).getTime(); }
        else if (tsKey === 'cachePct') {
          va = a.tokensIn > 0 ? a.cachedRead / a.tokensIn : 0;
          vb = b.tokensIn > 0 ? b.cachedRead / b.tokensIn : 0;
        }
        else { va = a[tsKey] || 0; vb = b[tsKey] || 0; }
        return (va - vb) * tsDir;
      });
      if (footer) {
        const filtered = consolidated.length;
        const total = historyState.selectedBucket ? historyState.totalRows : filtered;
        footer.textContent = filtered !== total ? `${filtered}/${total} rows` : (filtered === 1 ? '1 row' : `${filtered} rows`);
      }
      const tsIcon = (key) => {
        const active = historyState.tableSort.key === key;
        if (!active) return '';
        return tsDir < 0 ? ' <i class="bi bi-caret-down-fill" style="font-size:0.45rem"></i>' : ' <i class="bi bi-caret-up-fill" style="font-size:0.45rem"></i>';
      };
      const table = `<table class="table table-sm table-dark mb-0" style="font-size:0.65rem;background:transparent">
        <thead style="position:sticky;top:0;background:rgba(0,0,0,0.7)"><tr>
          <th class="history-table-th" data-sortkey="bucket" style="border-color:rgba(255,255,255,0.1);cursor:pointer;user-select:none">${historyState.granularity === 'day' ? 'Date' : 'Time'}${tsIcon('bucket')}</th>
          <th class="history-table-th" data-sortkey="requests" style="border-color:rgba(255,255,255,0.1);text-align:right;cursor:pointer;user-select:none" title="Actual requests → weighted requests (after priority adjustment)">Requests${tsIcon('requests')}</th>
          <th class="history-table-th" data-sortkey="tokensIn" style="border-color:rgba(255,255,255,0.1);text-align:right;cursor:pointer;user-select:none">Tokens In${tsIcon('tokensIn')}</th>
          <th class="history-table-th" data-sortkey="tokensOut" style="border-color:rgba(255,255,255,0.1);text-align:right;cursor:pointer;user-select:none">Tokens Out${tsIcon('tokensOut')}</th>
          <th class="history-table-th" data-sortkey="cachePct" style="border-color:rgba(255,255,255,0.1);text-align:right;cursor:pointer;user-select:none">Cache %${tsIcon('cachePct')}</th>
          <th class="history-table-th" data-sortkey="peak" style="border-color:rgba(255,255,255,0.1);text-align:right;cursor:pointer;user-select:none" title="Peak concurrent requests → peak weighted concurrent requests (after priority adjustment)">Peak${tsIcon('peak')}</th>
        </tr></thead>
        <tbody>${consolidated.map(c => {
          const t = new Date(c.bucket);
          const timeLabel = historyState.granularity === 'day' ? t.toLocaleDateString() : t.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' });
          const reqsCell = c.requests === c.weighted ? c.requests.toLocaleString() : c.requests.toLocaleString() + ' → ' + c.weighted.toLocaleString(undefined, { maximumFractionDigits: 1, minimumFractionDigits: 1 });
          const cachePct = c.tokensIn > 0 ? ((c.cachedRead / c.tokensIn) * 100).toFixed(1) + '%' : '--';
          // Build per-model detail rows
          const byModel = {};
          for (const r of c.rows) {
            const name = r.provider === r.model ? (r.model || '') : (r.provider && r.model ? r.model + ' → ' + r.provider : (r.provider || r.model || ''));
            if (!byModel[name]) byModel[name] = { requests: 0, weighted: 0, tokensIn: 0, tokensOut: 0, cachedRead: 0, peak: 0, peakWeighted: 0 };
            byModel[name].requests += r.requests || 0;
            byModel[name].weighted += r.weighted_requests || 0;
            byModel[name].tokensIn += r.tokens_in_total || r.tokens_in || 0;
            byModel[name].tokensOut += r.tokens_out || 0;
            byModel[name].cachedRead += r.tokens_cached_read || 0;
            const p = r.peak_concurrent_requests || 0;
            if (p > byModel[name].peak) byModel[name].peak = p;
            const pw = r.peak_weighted_concurrent_requests || 0;
            if (pw > byModel[name].peakWeighted) byModel[name].peakWeighted = pw;
          }
          const modelEntries = Object.entries(byModel);
          // Default sort: by current metric descending
          const defaultKey = historyState.metric === 'tokens' ? 'tokensIn' : 'requests';
          const sortKey = historyState.detailSort.key || defaultKey;
          const sortDir = historyState.detailSort.key ? historyState.detailSort.dir : -1;
          modelEntries.sort((a, b) => {
            if (sortKey === 'name') {
              return a[0] < b[0] ? -sortDir : a[0] > b[0] ? sortDir : 0;
            }
            let va, vb;
            if (sortKey === 'cachedRead') {
              va = a[1].tokensIn > 0 ? a[1].cachedRead / a[1].tokensIn : 0;
              vb = b[1].tokensIn > 0 ? b[1].cachedRead / b[1].tokensIn : 0;
            } else {
              va = a[1][sortKey] || 0;
              vb = b[1][sortKey] || 0;
            }
            return (va - vb) * sortDir;
          });
          const renderModelRows = (entries) => entries.map(([name, m]) => {
            const mReqsCell = m.requests === m.weighted ? m.requests.toLocaleString() : m.requests.toLocaleString() + ' → ' + m.weighted.toLocaleString(undefined, { maximumFractionDigits: 1, minimumFractionDigits: 1 });
            const mCachePct = m.tokensIn > 0 ? ((m.cachedRead / m.tokensIn) * 100).toFixed(1) + '%' : '--';
            return `<tr>
              <td style="border-color:rgba(255,255,255,0.03);color:rgba(255,255,255,0.7);padding-left:16px">${escapeHtml(name)}</td>
              <td style="border-color:rgba(255,255,255,0.03);text-align:right;font-variant-numeric:tabular-nums">${mReqsCell}</td>
              <td style="border-color:rgba(255,255,255,0.03);text-align:right;font-variant-numeric:tabular-nums">${formatCompact(m.tokensIn)}</td>
              <td style="border-color:rgba(255,255,255,0.03);text-align:right;font-variant-numeric:tabular-nums">${formatCompact(m.tokensOut)}</td>
              <td style="border-color:rgba(255,255,255,0.03);text-align:right;font-variant-numeric:tabular-nums">${mCachePct}</td>
              <td style="border-color:rgba(255,255,255,0.03);text-align:right;font-variant-numeric:tabular-nums">${m.peak === m.peakWeighted ? m.peak : m.peak + ' → ' + m.peakWeighted}</td>
            </tr>`;
          }).join('');
          // Sort indicator helper
          const sortIcon = (key) => {
            const active = historyState.detailSort.key === key || (!historyState.detailSort.key && key === defaultKey);
            const dir = active ? (historyState.detailSort.key ? historyState.detailSort.dir : -1) : 0;
            if (!active) return '';
            return dir < 0 ? ' <i class="bi bi-caret-down-fill" style="font-size:0.45rem"></i>' : ' <i class="bi bi-caret-up-fill" style="font-size:0.45rem"></i>';
          };
          return `<tr style="cursor:pointer" data-bucket="${escapeHtml(c.bucket)}">
            <td style="border-color:rgba(255,255,255,0.05)">${escapeHtml(timeLabel)} <i class="bi bi-chevron-down" style="font-size:0.5rem;margin-left:2px;transition:transform 0.2s ease"></i></td>
            <td style="border-color:rgba(255,255,255,0.05);text-align:right;font-variant-numeric:tabular-nums">${reqsCell}</td>
            <td style="border-color:rgba(255,255,255,0.05);text-align:right;font-variant-numeric:tabular-nums">${formatCompact(c.tokensIn)}</td>
            <td style="border-color:rgba(255,255,255,0.05);text-align:right;font-variant-numeric:tabular-nums">${formatCompact(c.tokensOut)}</td>
            <td style="border-color:rgba(255,255,255,0.05);text-align:right;font-variant-numeric:tabular-nums">${cachePct}</td>
            <td style="border-color:rgba(255,255,255,0.05);text-align:right;font-variant-numeric:tabular-nums">${c.peak === c.peakWeighted ? c.peak : c.peak + ' → ' + c.peakWeighted}</td>
          </tr>
          <tr class="history-detail-row" data-bucket="${escapeHtml(c.bucket)}" style="display:none">
            <td colspan="6" style="padding:0;border:none">
              <div class="history-detail-wrapper" style="overflow:hidden;max-height:0;transition:max-height 0.3s ease">
                <table class="table table-sm table-dark mb-0" style="font-size:0.6rem;background:rgba(0,0,0,0.15)">
                  <thead><tr>
                    <th class="history-detail-th" data-sortkey="name" style="border-color:rgba(255,255,255,0.05);font-weight:400;color:rgba(255,255,255,0.6);cursor:pointer;user-select:none">Model${sortIcon('name')}</th>
                    <th class="history-detail-th" data-sortkey="requests" style="border-color:rgba(255,255,255,0.05);font-weight:400;color:rgba(255,255,255,0.6);text-align:right;cursor:pointer;user-select:none">Requests${sortIcon('requests')}</th>
                    <th class="history-detail-th" data-sortkey="tokensIn" style="border-color:rgba(255,255,255,0.05);font-weight:400;color:rgba(255,255,255,0.6);text-align:right;cursor:pointer;user-select:none">Tokens In${sortIcon('tokensIn')}</th>
                    <th class="history-detail-th" data-sortkey="tokensOut" style="border-color:rgba(255,255,255,0.05);font-weight:400;color:rgba(255,255,255,0.6);text-align:right;cursor:pointer;user-select:none">Tokens Out${sortIcon('tokensOut')}</th>
                    <th class="history-detail-th" data-sortkey="cachedRead" style="border-color:rgba(255,255,255,0.05);font-weight:400;color:rgba(255,255,255,0.6);text-align:right;cursor:pointer;user-select:none">Cache %${sortIcon('cachedRead')}</th>
                    <th class="history-detail-th" data-sortkey="peak" style="border-color:rgba(255,255,255,0.05);font-weight:400;color:rgba(255,255,255,0.6);text-align:right;cursor:pointer;user-select:none">Peak${sortIcon('peak')}</th>
                  </tr></thead>
                  <tbody class="history-detail-tbody" data-models='${escapeHtml(JSON.stringify(modelEntries))}'>${renderModelRows(modelEntries)}</tbody>
                </table>
              </div>
            </td>
          </tr>`;
        }).join('')}</tbody>
      </table>`;
      container.innerHTML = table;
      // Click handler for outer table sort headers
      container.querySelectorAll('.history-table-th').forEach(th => {
        th.addEventListener('click', (e) => {
          e.stopPropagation();
          const key = th.dataset.sortkey;
          if (historyState.tableSort.key === key) {
            historyState.tableSort.dir *= -1;
          } else {
            historyState.tableSort.key = key;
            historyState.tableSort.dir = -1;
          }
          renderHistoryTable(groups);
        });
      });
      // Click handler for table rows: expand/collapse detail row
      container.querySelectorAll('tr[data-bucket]').forEach(tr => {
        tr.addEventListener('click', (e) => {
          e.stopPropagation();
          const bucket = tr.dataset.bucket;
          const detailRow = container.querySelector(`tr.history-detail-row[data-bucket="${CSS.escape(bucket)}"]`);
          if (!detailRow) return;
          const wrapper = detailRow.querySelector('.history-detail-wrapper');
          const chevron = tr.querySelector('.bi-chevron-down');
          const isExpanded = detailRow.style.display !== 'none' && wrapper.style.maxHeight !== '0px';
          if (isExpanded) {
            // Collapse: set explicit height first so transition has a starting value
            wrapper.style.maxHeight = wrapper.scrollHeight + 'px';
            wrapper.offsetHeight; // force reflow
            wrapper.style.maxHeight = '0px';
            if (chevron) chevron.style.transform = '';
            setTimeout(() => { detailRow.style.display = 'none'; }, 300);
          } else {
            // Expand
            detailRow.style.display = '';
            wrapper.style.maxHeight = wrapper.scrollHeight + 'px';
            if (chevron) chevron.style.transform = 'rotate(180deg)';
            setTimeout(() => { wrapper.style.maxHeight = 'none'; }, 300);
          }
        });
      });
      // Sort header click handlers for detail tables
      container.querySelectorAll('.history-detail-th').forEach(th => {
        th.addEventListener('click', (e) => {
          e.stopPropagation();
          const key = th.dataset.sortkey;
          if (historyState.detailSort.key === key) {
            historyState.detailSort.dir *= -1;
          } else {
            historyState.detailSort.key = key;
            historyState.detailSort.dir = key === 'name' ? 1 : -1;
          }
          const tbody = th.closest('table').querySelector('.history-detail-tbody');
          const entries = JSON.parse(tbody.dataset.models);
          const defaultKey = historyState.metric === 'tokens' ? 'tokensIn' : 'requests';
          const sortKey = historyState.detailSort.key || defaultKey;
          const sortDir = historyState.detailSort.key ? historyState.detailSort.dir : -1;
          entries.sort((a, b) => {
            if (sortKey === 'name') {
              return a[0] < b[0] ? -sortDir : a[0] > b[0] ? sortDir : 0;
            }
            let va, vb;
            if (sortKey === 'cachedRead') {
              // Cache % is a ratio, not a raw value — sort by the percentage
              va = a[1].tokensIn > 0 ? a[1].cachedRead / a[1].tokensIn : 0;
              vb = b[1].tokensIn > 0 ? b[1].cachedRead / b[1].tokensIn : 0;
            } else {
              va = a[1][sortKey] || 0;
              vb = b[1][sortKey] || 0;
            }
            return (va - vb) * sortDir;
          });
          // Re-render rows
          tbody.innerHTML = entries.map(([name, m]) => {
            const mReqsCell = m.requests === m.weighted ? m.requests.toLocaleString() : m.requests.toLocaleString() + ' → ' + m.weighted.toLocaleString(undefined, { maximumFractionDigits: 1, minimumFractionDigits: 1 });
            const mCachePct = m.tokensIn > 0 ? ((m.cachedRead / m.tokensIn) * 100).toFixed(1) + '%' : '--';
            return `<tr>
              <td style="border-color:rgba(255,255,255,0.03);color:rgba(255,255,255,0.7);padding-left:16px">${escapeHtml(name)}</td>
              <td style="border-color:rgba(255,255,255,0.03);text-align:right;font-variant-numeric:tabular-nums">${mReqsCell}</td>
              <td style="border-color:rgba(255,255,255,0.03);text-align:right;font-variant-numeric:tabular-nums">${formatCompact(m.tokensIn)}</td>
              <td style="border-color:rgba(255,255,255,0.03);text-align:right;font-variant-numeric:tabular-nums">${formatCompact(m.tokensOut)}</td>
              <td style="border-color:rgba(255,255,255,0.03);text-align:right;font-variant-numeric:tabular-nums">${mCachePct}</td>
              <td style="border-color:rgba(255,255,255,0.03);text-align:right;font-variant-numeric:tabular-nums">${m.peak === m.peakWeighted ? m.peak : m.peak + ' → ' + m.peakWeighted}</td>
            </tr>`;
          }).join('');
          // Update sort icons on all headers in this table
          th.closest('thead').querySelectorAll('.history-detail-th').forEach(h => {
            const hKey = h.dataset.sortkey;
            const active = historyState.detailSort.key === hKey || (!historyState.detailSort.key && hKey === defaultKey);
            const dir = active ? (historyState.detailSort.key ? historyState.detailSort.dir : -1) : 0;
            // Rebuild header text + icon (strip old <i> caret elements, not unicode chars)
            const baseLabel = hKey === 'name' ? 'Model' : hKey === 'requests' ? 'Requests' : hKey === 'tokensIn' ? 'Tokens In' : hKey === 'tokensOut' ? 'Tokens Out' : 'Cache %';
            h.innerHTML = baseLabel + (active ? (dir < 0 ? ' <i class="bi bi-caret-down-fill" style="font-size:0.45rem"></i>' : ' <i class="bi bi-caret-up-fill" style="font-size:0.45rem"></i>') : '');
          });
        });
      });
      // Auto-expand detail row when a bar is selected
      if (historyState.selectedBucket) {
        const tr = container.querySelector(`tr[data-bucket="${CSS.escape(historyState.selectedBucket)}"]`);
        if (tr) tr.click();
      }
    }
    function renderHistory() {
      const data = lastHistoryData || {};
      const buckets = data.buckets || [];
      const groups = groupHistoryBuckets(buckets);
      const totals = aggregateHistoryTotals(buckets);
      document.getElementById('histTotalRequests').textContent = formatCompact(totals.requests);
      document.getElementById('histCachedPct').textContent = totals.cachedPct.toFixed(1) + '%';
      document.getElementById('histErrorRate').textContent = totals.errorRate.toFixed(1) + '%';
      document.getElementById('histPeakConc').textContent = totals.peakConc === totals.peakWeighted ? totals.peakConc.toString() : totals.peakConc.toString() + ' → ' + totals.peakWeighted.toString();
      document.getElementById('histTokensIn').textContent = formatCompact(totals.tokensIn);
      document.getElementById('histTokensOut').textContent = formatCompact(totals.tokensOut);
      renderHistoryChart(groups);
      historyState.totalRows = groups.flatMap(g => g.rows).length;
      if (!historyState.selectedBucket) renderHistoryTable(groups);
    }
    function setRefreshInterval(seconds, skipImmediate) {
      if (usageIntervalId) clearInterval(usageIntervalId);
      if (historyIntervalId) clearInterval(historyIntervalId);
      const ms = parseInt(seconds) * 1000;
      localStorage.setItem('refreshInterval', seconds.toString());
      if (!skipImmediate) {
        fetchUsage(); fetchConcurrency(); updateStatus(); fetchHistory();
      }
      // Single unified cycle: status + usage + concurrency + history all refresh together
      usageIntervalId = setInterval(() => { updateStatus(); fetchUsage(); fetchConcurrency(); }, ms);
      historyIntervalId = setInterval(() => { fetchHistory(); }, ms);
      document.querySelectorAll('.btn-refresh').forEach(btn => {
        const isActive = btn.dataset.refresh === seconds.toString();
        btn.classList.toggle('active', isActive);
        btn.setAttribute('aria-pressed', isActive ? 'true' : 'false');
      });
    }
    async function checkHealth() {
      try{const r=await fetch('/healthz');const d=await r.json();if(d.ok){proxyData=d;document.getElementById('statusIndicator').className='status-indicator status-online';renderTokenPools();updateStats();showToast('Health OK');}else throw Error();}catch{document.getElementById('statusIndicator').className='status-indicator status-offline';showToast('Health check failed','danger');}
    }
    async function testConnection() {
      try{const r=await fetch('/v1/models');if(r.ok){const d=await r.json();showToast('Connected! {n} models'.replace('{n}', d.data?.length||0));}else throw Error();}catch{showToast('Connection test failed','danger');}
    }
    function updateLastUpdated() {
      const el = document.getElementById('lastUpdated');
      if (el) el.textContent = 'Updated ' + new Date().toLocaleTimeString(undefined, {hour:'2-digit',minute:'2-digit',second:'2-digit'});
    }
    function updateStatus() {
      fetch('/healthz').then(r=>r.json()).then(d=>{proxyData=d;document.getElementById('statusIndicator').className=d.ok?'status-indicator status-online':'status-indicator status-offline';renderTokenPools();updateStats();updateLastUpdated();}).catch(()=>{document.getElementById('statusIndicator').className='status-indicator status-offline';});
    }

    function restartProxy() {
      if (!confirm('Restart the proxy?')) return;
      const btn = document.getElementById('restartBtn');
      btn.disabled = true; btn.innerHTML = '<i class="bi bi-arrow-counterclockwise me-2"></i>' + 'Restarting...';
      document.getElementById('statusIndicator').className = 'status-indicator status-reconnecting';
      fetch('/api/restart', { method: 'POST' }).then(r => r.json()).then(d => showToast(d.message || 'Restarting...')).catch(() => {});
      let attempts = 0;
      const iv = setInterval(() => {
        attempts++;
        if (attempts >= 30) {
          document.getElementById('statusIndicator').className = 'status-indicator status-offline';
          btn.disabled = false; btn.innerHTML = '<i class="bi bi-arrow-counterclockwise me-2"></i>' + 'Restart Proxy';
          showToast('Proxy did not come back', 'danger'); clearInterval(iv);
          return;
        }
        fetch('/healthz').then(r => r.json()).then(d => {
          if (d.ok) {
            document.getElementById('statusIndicator').className = 'status-indicator status-online';
            btn.disabled = false; btn.innerHTML = '<i class="bi bi-arrow-counterclockwise me-2"></i>' + 'Restart Proxy';
            showToast('Proxy is back online!', 'success'); clearInterval(iv);
          }
        }).catch(() => {});
      }, 2000);
    }

    let resizeTimer;
    window.addEventListener('resize', () => { clearTimeout(resizeTimer); resizeTimer = setTimeout(initLiquidGlass, 150); });

    document.addEventListener('DOMContentLoaded', async () => {
      hideDashboardUntilWallpaper();
      // Sections marked collapsed in HTML should be fully hidden from the start
      document.querySelectorAll('.collapse-section.collapsed').forEach(s => { s.style.display='none'; });
      document.querySelectorAll('[data-bs-toggle="tooltip"]').forEach(el => new bootstrap.Tooltip(el));
      loadConfig();
      updateStatus();
      fetchConcurrency();
      setTimeout(() => { setHistoryRange('7d'); }, 0);
      document.querySelectorAll('.btn-hist-gran').forEach(btn => btn.classList.toggle('active', btn.dataset.gran === historyState.granularity));
      setHistoryMetric(historyState.metric);
      // Restore persisted refresh interval (default 30s).
      // skipImmediate=true because loadConfig already fetches usage/concurrency,
      // and setHistoryRange already fetches history.
      const savedInterval = localStorage.getItem('refreshInterval') || '30';
      setRefreshInterval(savedInterval, true);
      document.getElementById('modelsContainer').addEventListener('click', (e) => {
        const tag = e.target.closest('.model-tag');
        if (tag && tag.dataset.model) toggleModel(tag.dataset.model);
      });
      setTimeout(()=>window.dispatchEvent(new Event('resize')),1000);
      setTimeout(()=>window.dispatchEvent(new Event('resize')),3000);
      window.addEventListener('resize', layoutStatGrids);
      layoutStatGrids();
    });
    function hideDashboardUntilWallpaper() {
      if (document.getElementById('wallpaperLoader')) return;
      const loader = document.createElement('div');
      loader.id = 'wallpaperLoader';
      loader.style.cssText = 'position:fixed;inset:0;z-index:99999;background:transparent;display:flex;align-items:center;justify-content:center;flex-direction:column;gap:16px;color:rgba(255,255,255,0.8);transition:opacity 0.4s ease';
      loader.innerHTML = '<div class=\"spinner-border text-light\" role=\"status\"></div><div style=\"font-size:14px;letter-spacing:0.5px\">Loading dashboard...</div>';
      document.body.appendChild(loader);
    }