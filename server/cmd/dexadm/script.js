async function requestJSON (method, addr, opts) {
  opts = opts || {}
  const req = {
    method: method,
    headers: {  'Content-Type': opts.contentType ?? 'application/json' },
    body: opts.reqBody
  }
  const resp = await window.fetch(addr, req)
  const body = await resp.text()
  if (resp.status !== 200) { console.log(resp); throw new Error(`${resp.status}: ${resp.statusText} : ${body}`) }
  if (body.length === 0) return "OK"
  if (opts.raw) return body
  return JSON.parse(body)
}

(async () => {
  const page = {}
  for (const el of document.querySelectorAll('[id]')) page[el.id] = el

  page.responseTmpl.remove()
  page.responseTmpl.removeAttribute('id')

  const writeResult = (path, res, isError) => {
    const div = page.responseTmpl.cloneNode(true)
    const tmpl = Array.from(div.querySelectorAll('[data-tmpl]')).reduce((d, el) => {
      d[el.dataset.tmpl] = el
      return d
    }, {})
    page.responses.prepend(div)
    tmpl.path.textContent = path
    tmpl.close.addEventListener('click', () => div.remove())
    tmpl.response.textContent = res
    if (isError) tmpl.response.classList.add('errcolor')
    while (page.responses.children.length > 20) page.responses.removeChild(page.respones.lastChild)
    page.responses.scrollTo(0, 0)
  }

  const doRequest = async (method, path, opts) => {
    try {
      const resp = await requestJSON(method, path, opts)
      if (opts?.raw) tmpl.response.textContent = resp
      else writeResult(path, JSON.stringify(resp, null, 4))
    } catch (e) {
      writeResult(path, e.toString(), true)
    }
  }

  const get = (path, opts) => doRequest('GET', path, opts)
  const post = (path, reqBody, contentType) => doRequest('POST', path, { reqBody, contentType })

  page.assetBttn.addEventListener('click', () => get(`/asset/${page.assetInput.value}`))
  page.feeScaleBttn.addEventListener('click', () => get(`/asset/${page.assetInput.value}/setfeescale/${page.feeScaleInput.value}`))
  page.configBttn.addEventListener('click', () => get('/config'))
  page.listAccountsBttn.addEventListener('click', () => get('/accounts'))
  page.accountInfoBttn.addEventListener('click', () => get(`/account/${page.accountIDInput.value}`))
  page.accountOutcomesBttn.addEventListener('click', () => get(`/account/${page.accountIDInput.value}/outcomes?n=100`))
  page.matchFailsBttn.addEventListener('click', () => get(`/account/${page.accountIDInput.value}/fails?n=100`))
  page.forgiveMatchBttn.addEventListener('click', () => get(`/account/${page.accountIDInput.value}/forgive_match/${page.forgiveMatchIDInput.value}`))
  page.forgiveUserBttn.addEventListener('click', () => get(`/account/${page.accountIDInput.value}/forgive_user`))
  page.notifyAccountBttn.addEventListener('click', () => post(`/account/${page.accountIDInput.value}/notify`, page.notifyAccountInput.value, 'text/plain'))
  page.broadcastBttn.addEventListener('click', () => post(`/notifyall`, page.broadcastInput.value, 'text/plain'))
  page.viewMarketsBttn.addEventListener('click', () => get('/markets'))
  page.marketInfoBttn.addEventListener('click', () => get(`/market/${page.marketIDInput.value}`))
  page.marketBookBttn.addEventListener('click', () => get(`/market/${page.marketIDInput.value}/orderbook`))
  page.marketEpochBttn.addEventListener('click', () => get(`/market/${page.marketIDInput.value}/epochorders`))
  page.marketMatchesBttn.addEventListener('click', () => {
    const uri = `/market/${page.marketIDInput.value}/matches?n=100&includeinactive=${page.includeInactiveMatches.checked ? 'true' : 'false'}`
    get(uri, { raw: true })
  })
  page.suspendTimeCheckbox.addEventListener('change', () => page.suspendTimeInput.classList.toggle('d-none', !page.suspendTimeCheckbox.checked))
  page.unsuspendTimeCheckbox.addEventListener('change', () => page.unsuspendTimeInput.classList.toggle('d-none', !page.unsuspendTimeCheckbox.checked))
  const susun = (tag, withTime, timeV) => {
    if (!page.marketIDInput.value) return writeResult('/market', "no market specified", true)
    if (withTime && timeV === '') return writeResult('/market', "datetime not set", true)
    const params = new URLSearchParams()
    if (tag === 'suspend') params.append('persist', `${page.persistBook.checked ? 'true' : 'false'}`)
    if (withTime && timeV) params.append('t', (new Date(timeV)).getTime())
    get(`/market/${page.marketIDInput.value}/${tag}?${params.toString()}`)
  }
  page.suspendBttn.addEventListener('click', () => susun('suspend', page.suspendTimeCheckbox.checked, page.suspendTimeInput.value))
  page.resumeBttn.addEventListener('click', () => susun('resume', page.unsuspendTimeCheckbox.checked, page.unsuspendTimeInput.value))
  page.generatePrepaidBondsBttn.addEventListener('click', () => {
    const [n, days, strength] = [page.prepaidBondCountInput.value, page.prepaidBondDaysInput.value, page.prepaidBondStrengthInput.value]
    get(`/prepaybonds?n=${n}&days=${days}&strength=${strength}`)
  })

  // Mesh status: structured left-panel renderer + optional Watch polling.
  // Status must not use plain get()/doRequest — those only dump history.
  const MESH_POLL_MS = 1500
  const MESH_LAG_WARN = 1
  const MESH_LAG_ERR = 100

  const formatAge = (ms) => {
    if (!Number.isFinite(ms) || ms < 0) return '—'
    const s = Math.floor(ms / 1000)
    if (s < 60) return `${s}s ago`
    const m = Math.floor(s / 60)
    const rs = s % 60
    if (m < 60) return `${m}m${rs}s ago`
    const h = Math.floor(m / 60)
    return `${h}h${m % 60}m ago`
  }

  const truncHash = (h) => {
    if (!h) return '—'
    return h.length > 12 ? h.slice(0, 12) + '…' : h
  }

  const setText = (el, text) => { el.textContent = text }
  const show = (el, on) => el.classList.toggle('d-none', !on)

  const applyMeshStatus = (st) => {
    const mode = st.mode || '—'
    const single = mode === 'single_server'

    setText(page.meshMode, mode)
    page.meshMode.classList.toggle('errcolor', mode === 'halted')
    page.meshMode.classList.toggle('warncolor', mode === 'slave_no_master')

    setText(page.meshReady, st.ready ? 'true' : 'false')
    page.meshReady.classList.toggle('okcolor', !!st.ready)
    page.meshReady.classList.toggle('errcolor', !st.ready)

    // Strict clean single_server: only mode + ready (+ halt below).
    show(page.meshLastTransitionRow, !single)
    show(page.meshIdentityBlock, !single)
    show(page.meshFrontierBlock, !single)
    show(page.meshDialBlock, !single)
    show(page.meshCmdsBlock, !single)

    if (!single) {
      const lt = st.lastTransition ? Date.parse(st.lastTransition) : NaN
      setText(page.meshLastTransition,
        st.lastTransition ? `${st.lastTransition} (${formatAge(Date.now() - lt)})` : '—')

      setText(page.meshNodeID, st.nodeID || '—')
      const peerTxt = st.peerConnected
        ? `connected ${st.peerNodeID || ''}`.trim()
        : 'disconnected'
      setText(page.meshPeer, peerTxt)
      const peerExpected = ['established_master', 'established_slave',
        'established_slave_syncing', 'preparing_master'].includes(mode)
      page.meshPeer.classList.toggle('errcolor', peerExpected && !st.peerConnected)
      page.meshPeer.classList.toggle('okcolor', !!st.peerConnected)

      setText(page.meshFrontier,
        st.frontierSeq != null
          ? `${st.frontierSeq}  ${truncHash(st.frontierHash)}`
          : '—')
      page.meshFrontier.title = st.frontierHash || ''

      setText(page.meshDialAttempts, String(st.dialAttempts ?? 0))
      // Last dial error is diagnostic for reconnect failures; hide once peer is up.
      show(page.meshDialErrRow, !!st.lastDialError && !st.peerConnected)
      setText(page.meshLastDialError, st.lastDialError
        ? `${st.lastDialError}${st.lastDialAt ? ' @ ' + st.lastDialAt : ''}`
        : '')

      setText(page.meshPendingCmds, String(st.pendingForwardedCommands ?? 0))
      setText(page.meshStateLoaded, st.stateLoaded ? 'true' : 'false')
    }

    show(page.meshHaltRow, !!st.haltErr)
    setText(page.meshHaltErr, st.haltErr || '')

    const failover = mode === 'slave_no_master'
    show(page.meshFailoverBlock, failover)
    if (failover) {
      setText(page.meshDisconnectedAt, st.peerDisconnectedAt || '—')
      setText(page.meshPromoteAt, st.promoteAt || '—')
      // Poll-quantized: only as accurate as last successful apply.
      const promoteMs = st.promoteAt ? Date.parse(st.promoteAt) - Date.now() : NaN
      const sec = Math.max(0, Math.ceil(promoteMs / 1000))
      setText(page.meshPromoteIn, Number.isFinite(promoteMs) ? `${sec}s` : '—')
      page.meshPromoteIn.classList.toggle('errcolor', sec > 0 && sec <= 5)
      page.meshPromoteIn.classList.toggle('warncolor', sec > 5)
    }

    const master = mode === 'established_master'
    show(page.meshStreamBlock, master)
    if (master) {
      const lag = st.streamLag || 0
      setText(page.meshStream,
        `${st.streamActive ? 'active' : 'idle'}  tip=${st.streamTip ?? 0}  cursor=${st.streamCursor ?? 0}  lag=${lag}`)
      page.meshStream.classList.toggle('warncolor', lag >= MESH_LAG_WARN && lag < MESH_LAG_ERR)
      page.meshStream.classList.toggle('errcolor', lag >= MESH_LAG_ERR)
      setText(page.meshPendingStreamResults, String(st.pendingStreamResults ?? 0))
    }

    // Server truth only — not "all transitional modes".
    show(page.meshSeedingRow, !!st.seeding)

    show(page.meshErr, false)
    setText(page.meshUpdated, `updated ${new Date().toLocaleTimeString()}`)
  }

  let meshTimer = null
  // Shared in-flight promise: watch skips; Status awaits and always dumps history.
  let meshFetchPromise = null

  const runMeshFetch = () => {
    if (meshFetchPromise) return meshFetchPromise
    meshFetchPromise = (async () => {
      try {
        const st = await requestJSON('GET', '/mesh')
        applyMeshStatus(st)
        return { ok: true, st }
      } catch (e) {
        show(page.meshErr, true)
        setText(page.meshErr, e.toString())
        return { ok: false, err: e }
      } finally {
        meshFetchPromise = null
      }
    })()
    return meshFetchPromise
  }

  // Watch tick: skip entirely if a request is already in flight.
  const meshWatchTick = () => {
    if (meshFetchPromise) return
    runMeshFetch() // no history
  }

  // Status: always completes with history; never a silent no-op.
  const fetchMeshStatusToHistory = async () => {
    const result = await runMeshFetch()
    if (result.ok) writeResult('/mesh', JSON.stringify(result.st, null, 4))
    else writeResult('/mesh', result.err.toString(), true)
  }

  const clearMeshTimer = () => {
    if (meshTimer) clearInterval(meshTimer)
    meshTimer = null
  }

  const stopMeshWatch = () => {
    clearMeshTimer()
    setText(page.meshWatchMeta, '')
  }

  const startMeshWatch = () => {
    clearMeshTimer()
    setText(page.meshWatchMeta, `polling ${MESH_POLL_MS}ms`)
    meshWatchTick()
    meshTimer = setInterval(meshWatchTick, MESH_POLL_MS)
  }

  const pauseMeshWatch = () => {
    clearMeshTimer()
    setText(page.meshWatchMeta, 'paused (tab hidden)')
  }

  page.meshStatusBttn.addEventListener('click', () => fetchMeshStatusToHistory())
  page.meshWatchCheckbox.addEventListener('change', () => {
    if (page.meshWatchCheckbox.checked) startMeshWatch()
    else stopMeshWatch()
  })

  document.addEventListener('visibilitychange', () => {
    if (!page.meshWatchCheckbox.checked) return
    if (document.visibilityState === 'hidden') pauseMeshWatch()
    else startMeshWatch()
  })
})()
