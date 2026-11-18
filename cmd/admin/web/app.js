(() => {
  'use strict';

  // Binary protocol constants (matching internal/protocol/protocol.go)
  const MSG_MOUSE = 0x30;
  const MSG_KEY = 0x31;

  const MOUSE_MOVE = 0;
  const MOUSE_DOWN = 1;
  const MOUSE_UP = 2;
  const MOUSE_WHEEL = 3;
  const MOUSE_HWHEEL = 4;

  const KEY_DOWN = 0;
  const KEY_UP = 1;
  const KEY_UNICODE = 2;
  const KEY_FLAG_EXTENDED = 1;

  // DOM elements
  const dashboardView = document.getElementById('dashboard-view');
  const viewerView = document.getElementById('viewer-view');
  const serverBadge = document.getElementById('server-badge');
  const wsStatus = document.getElementById('ws-status');
  const clientCount = document.getElementById('client-count');
  const clientList = document.getElementById('client-list');
  const emptyState = document.getElementById('empty-state');

  const notice = document.getElementById('notice');
  const noticeText = document.getElementById('notice-text');
  const noticeClose = document.getElementById('notice-close');

  const pendingModal = document.getElementById('pending-modal');
  const pendingHostname = document.getElementById('pending-hostname');
  const btnCancelPending = document.getElementById('btn-cancel-pending');

  const viewerHostname = document.getElementById('viewer-hostname');
  const viewerStats = document.getElementById('viewer-stats');
  const qualitySelect = document.getElementById('quality-select');
  const btnFullscreen = document.getElementById('btn-fullscreen');
  const btnDisconnect = document.getElementById('btn-disconnect');
  const canvasStage = document.getElementById('canvas-stage');
  const canvas = document.getElementById('remote-canvas');
  const ctx = canvas.getContext('2d', { alpha: false });

  const genModal = document.getElementById('gen-modal');
  const btnOpenGen = document.getElementById('btn-open-gen');
  const btnCloseGen = document.getElementById('btn-close-gen');
  const btnDownloadClient = document.getElementById('btn-download-client');
  const genServerAddr = document.getElementById('gen-server-addr');
  const ipChips = document.getElementById('ip-chips');
  const genError = document.getElementById('gen-error');

  // Application state
  let ws = null;
  let serverInfo = null;
  let activeSession = null; // { sessionId, hostname, width, height }
  let pendingSession = null;
  let frameCounter = 0;
  let lastFpsTick = performance.now();
  let currentFps = 0;
  let lastMoveSent = 0;
  const pressedVKs = new Map(); // code -> { vk, flags }

  // --- Notifications ---
  let noticeTimer = null;
  function showNotice(msg, isError = false) {
    clearTimeout(noticeTimer);
    noticeText.textContent = msg;
    notice.classList.toggle('notice-error', isError);
    notice.classList.remove('hidden');
    noticeTimer = setTimeout(() => notice.classList.add('hidden'), 6000);
  }

  noticeClose.addEventListener('click', () => notice.classList.add('hidden'));

  // --- Load server info ---
  async function loadServerInfo() {
    try {
      const res = await fetch('/api/info');
      if (!res.ok) return;
      serverInfo = await res.json();
      serverBadge.textContent = `Puerto ${serverInfo.agent_port} · TLS 1.3 · ${serverInfo.cert_fingerprint}`;
      if (!genServerAddr.value && serverInfo.default_address) {
        genServerAddr.value = serverInfo.default_address;
      }
      renderIPChips();
    } catch {
      // ignore transient error
    }
  }

  function renderIPChips() {
    ipChips.innerHTML = '';
    if (!serverInfo) return;
    const candidates = [...(serverInfo.local_ips || []), '127.0.0.1'];
    const port = serverInfo.agent_port || 9443;
    const seen = new Set();
    for (const ip of candidates) {
      if (seen.has(ip)) continue;
      seen.add(ip);
      const addr = `${ip}:${port}`;
      const btn = document.createElement('button');
      btn.type = 'button';
      btn.className = 'chip';
      btn.textContent = ip === '127.0.0.1' ? `${addr} (local)` : addr;
      btn.addEventListener('click', () => {
        genServerAddr.value = addr;
        genError.classList.add('hidden');
      });
      ipChips.appendChild(btn);
    }
  }

  // --- WebSocket connection ---
  function connectWS() {
    const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
    wsStatus.textContent = 'Conectando...';
    ws = new WebSocket(`${proto}//${location.host}/ws`);
    ws.binaryType = 'arraybuffer';

    ws.addEventListener('open', () => {
      wsStatus.textContent = 'Panel conectado';
    });

    ws.addEventListener('close', () => {
      wsStatus.textContent = 'Reconectando...';
      if (activeSession) {
        closeViewer('Se perdió la conexión con el panel local.');
      }
      if (pendingSession) {
        pendingSession = null;
        pendingModal.classList.add('hidden');
      }
      setTimeout(connectWS, 1500);
    });

    ws.addEventListener('message', (ev) => {
      if (ev.data instanceof ArrayBuffer) {
        handleBinaryFrame(ev.data);
        return;
      }
      try {
        handleControlMessage(JSON.parse(ev.data));
      } catch {
        // ignore malformed message
      }
    });
  }

  function sendJSON(obj) {
    if (ws && ws.readyState === WebSocket.OPEN) {
      ws.send(JSON.stringify(obj));
    }
  }

  function handleControlMessage(msg) {
    switch (msg.type) {
      case 'clients_update':
        renderClients(msg.clients || []);
        break;

      case 'session_pending':
        pendingSession = { sessionId: msg.session_id, hostname: msg.hostname };
        pendingHostname.textContent = msg.hostname;
        pendingModal.classList.remove('hidden');
        break;

      case 'session_started':
        pendingSession = null;
        pendingModal.classList.add('hidden');
        openViewer(msg);
        break;

      case 'session_denied':
        pendingSession = null;
        pendingModal.classList.add('hidden');
        showNotice(`${msg.hostname}: ${msg.reason || 'Solicitud denegada.'}`, true);
        break;

      case 'session_ended':
        pendingSession = null;
        pendingModal.classList.add('hidden');
        if (activeSession) {
          closeViewer(msg.reason || 'Sesión finalizada.');
        }
        break;

      case 'error':
        pendingSession = null;
        pendingModal.classList.add('hidden');
        showNotice(msg.message || 'Ocurrió un error.', true);
        break;
    }
  }

  // --- Client list rendering ---
  function renderClients(clients) {
    clientCount.textContent = String(clients.length);
    clientList.innerHTML = '';

    if (clients.length === 0) {
      emptyState.classList.remove('hidden');
      return;
    }
    emptyState.classList.add('hidden');

    for (const c of clients) {
      const row = document.createElement('div');
      row.className = 'client-row' + (c.status !== 'online' ? ' is-busy' : '');

      const info = document.createElement('div');
      info.className = 'client-info';

      const dot = document.createElement('span');
      dot.className = 'dot ' + (
        c.status === 'active' ? 'dot-active' :
        c.status === 'pending' ? 'dot-pending' : 'dot-online'
      );

      const textWrap = document.createElement('div');
      textWrap.className = 'client-text';

      const nameEl = document.createElement('div');
      nameEl.className = 'client-name';
      nameEl.textContent = c.hostname;

      const metaEl = document.createElement('div');
      metaEl.className = 'client-meta';
      const resStr = c.width && c.height ? ` · ${c.width}×${c.height}` : '';
      metaEl.textContent = `${c.ip}${resStr}`;

      textWrap.appendChild(nameEl);
      textWrap.appendChild(metaEl);
      info.appendChild(dot);
      info.appendChild(textWrap);

      const actions = document.createElement('div');
      actions.className = 'client-actions';

      const statusEl = document.createElement('span');
      statusEl.className = 'status-label';
      statusEl.textContent =
        c.status === 'active' ? 'En sesión' :
        c.status === 'pending' ? 'Esperando aprobación' : 'Disponible';

      const btn = document.createElement('button');
      btn.type = 'button';
      btn.className = 'btn btn-secondary btn-sm';
      btn.textContent = 'Conectar';
      btn.disabled = c.status !== 'online';
      btn.addEventListener('click', (e) => {
        e.stopPropagation();
        requestConnect(c.id);
      });

      actions.appendChild(statusEl);
      actions.appendChild(btn);

      row.appendChild(info);
      row.appendChild(actions);

      if (c.status === 'online') {
        row.addEventListener('click', () => requestConnect(c.id));
      }
      clientList.appendChild(row);
    }
  }

  function requestConnect(clientId) {
    notice.classList.add('hidden');
    sendJSON({ type: 'connect', client_id: clientId });
  }

  btnCancelPending.addEventListener('click', () => {
    sendJSON({ type: 'cancel' });
    pendingSession = null;
    pendingModal.classList.add('hidden');
  });

  // --- Remote Viewer ---
  function openViewer(msg) {
    activeSession = {
      sessionId: msg.session_id,
      hostname: msg.hostname,
      width: msg.width || 1280,
      height: msg.height || 720,
    };
    canvas.width = activeSession.width;
    canvas.height = activeSession.height;
    ctx.fillStyle = '#000000';
    ctx.fillRect(0, 0, canvas.width, canvas.height);

    viewerHostname.textContent = activeSession.hostname;
    frameCounter = 0;
    currentFps = 0;
    lastFpsTick = performance.now();
    updateViewerStats();

    dashboardView.classList.add('hidden');
    viewerView.classList.remove('hidden');
    canvasStage.focus();

    sendJSON({
      type: 'stream_config',
      quality: Number(qualitySelect.value) || 65,
      keyframe: true,
    });
  }

  function closeViewer(reason) {
    releaseAllPressedKeys();
    activeSession = null;
    if (document.fullscreenElement) {
      document.exitFullscreen().catch(() => {});
    }
    viewerView.classList.add('hidden');
    dashboardView.classList.remove('hidden');
    if (reason) {
      showNotice(reason, false);
    }
  }

  function updateViewerStats() {
    if (!activeSession) return;
    viewerStats.textContent = `${activeSession.width}×${activeSession.height} · ${currentFps} fps`;
  }

  btnDisconnect.addEventListener('click', () => {
    sendJSON({ type: 'disconnect' });
    closeViewer('Sesión cerrada.');
  });

  qualitySelect.addEventListener('change', () => {
    if (!activeSession) return;
    sendJSON({
      type: 'stream_config',
      quality: Number(qualitySelect.value) || 65,
      keyframe: true,
    });
    canvasStage.focus();
  });

  btnFullscreen.addEventListener('click', () => {
    if (!document.fullscreenElement) {
      viewerView.requestFullscreen().catch(() => {});
    } else {
      document.exitFullscreen().catch(() => {});
    }
    canvasStage.focus();
  });

  // --- Binary tile decoder & painter ---
  let decodingQueue = Promise.resolve();

  function handleBinaryFrame(buffer) {
    if (!activeSession || buffer.byteLength < 6) return;
    const view = new DataView(buffer);
    const screenW = view.getUint16(0, false);
    const screenH = view.getUint16(2, false);
    const tileCount = view.getUint16(4, false);
    if (screenW === 0 || screenH === 0 || tileCount === 0) return;

    const tiles = [];
    let offset = 6;
    for (let i = 0; i < tileCount; i++) {
      if (offset + 12 > buffer.byteLength) break;
      const x = view.getUint16(offset, false);
      const y = view.getUint16(offset + 2, false);
      const w = view.getUint16(offset + 4, false);
      const h = view.getUint16(offset + 6, false);
      const len = view.getUint32(offset + 8, false);
      offset += 12;
      if (offset + len > buffer.byteLength) break;
      const blob = new Blob([new Uint8Array(buffer, offset, len)], { type: 'image/jpeg' });
      tiles.push({ x, y, w, h, blob });
      offset += len;
    }
    if (tiles.length === 0) return;

    // Decode tiles concurrently, paint in arrival order.
    const decodeAll = Promise.all(
      tiles.map((t) => createImageBitmap(t.blob).then((bmp) => ({ t, bmp })))
    );

    decodingQueue = decodingQueue.then(async () => {
      const decoded = await decodeAll;
      if (!activeSession) {
        decoded.forEach(({ bmp }) => bmp.close());
        return;
      }
      if (canvas.width !== screenW || canvas.height !== screenH) {
        canvas.width = screenW;
        canvas.height = screenH;
        activeSession.width = screenW;
        activeSession.height = screenH;
      }
      for (const { t, bmp } of decoded) {
        ctx.drawImage(bmp, t.x, t.y, t.w, t.h);
        bmp.close();
      }
      frameCounter++;
      const now = performance.now();
      if (now - lastFpsTick >= 1000) {
        currentFps = Math.round((frameCounter * 1000) / (now - lastFpsTick));
        frameCounter = 0;
        lastFpsTick = now;
        updateViewerStats();
      }
    }).catch(() => {});
  }

  // --- Mouse Input Forwarding ---
  function sendMouse(action, button, normX, normY, delta = 0) {
    if (!activeSession || !ws || ws.readyState !== WebSocket.OPEN) return;
    const buf = new ArrayBuffer(9);
    const dv = new DataView(buf);
    dv.setUint8(0, MSG_MOUSE);
    dv.setUint8(1, action);
    dv.setUint8(2, button);
    dv.setUint16(3, Math.max(0, Math.min(65535, Math.round(normX * 65535))), false);
    dv.setUint16(5, Math.max(0, Math.min(65535, Math.round(normY * 65535))), false);
    dv.setInt16(7, delta, false);
    ws.send(buf);
  }

  function mapCanvasCoords(e) {
    const rect = canvas.getBoundingClientRect();
    if (rect.width <= 0 || rect.height <= 0) return { x: 0, y: 0 };
    const x = Math.max(0, Math.min(1, (e.clientX - rect.left) / rect.width));
    const y = Math.max(0, Math.min(1, (e.clientY - rect.top) / rect.height));
    return { x, y };
  }

  function mapMouseButton(btn) {
    if (btn === 2) return 1; // right
    if (btn === 1) return 2; // middle
    return 0; // left
  }

  canvas.addEventListener('mousemove', (e) => {
    if (!activeSession) return;
    const now = performance.now();
    if (now - lastMoveSent < 16) return;
    lastMoveSent = now;
    const { x, y } = mapCanvasCoords(e);
    sendMouse(MOUSE_MOVE, 0, x, y, 0);
  });

  canvas.addEventListener('mousedown', (e) => {
    if (!activeSession) return;
    e.preventDefault();
    canvasStage.focus();
    const { x, y } = mapCanvasCoords(e);
    sendMouse(MOUSE_DOWN, mapMouseButton(e.button), x, y, 0);
  });

  canvas.addEventListener('mouseup', (e) => {
    if (!activeSession) return;
    e.preventDefault();
    const { x, y } = mapCanvasCoords(e);
    sendMouse(MOUSE_UP, mapMouseButton(e.button), x, y, 0);
  });

  canvas.addEventListener('contextmenu', (e) => {
    if (activeSession) e.preventDefault();
  });

  canvas.addEventListener('wheel', (e) => {
    if (!activeSession) return;
    e.preventDefault();
    const { x, y } = mapCanvasCoords(e);
    if (Math.abs(e.deltaY) >= Math.abs(e.deltaX)) {
      const steps = e.deltaY > 0 ? -120 : 120;
      sendMouse(MOUSE_WHEEL, 0, x, y, steps);
    } else if (e.deltaX !== 0) {
      const steps = e.deltaX > 0 ? 120 : -120;
      sendMouse(MOUSE_HWHEEL, 0, x, y, steps);
    }
  }, { passive: false });

  // --- Keyboard Input Forwarding ---
  function sendKey(action, vk, scan, flags) {
    if (!activeSession || !ws || ws.readyState !== WebSocket.OPEN) return;
    const buf = new ArrayBuffer(7);
    const dv = new DataView(buf);
    dv.setUint8(0, MSG_KEY);
    dv.setUint8(1, action);
    dv.setUint16(2, vk, false);
    dv.setUint16(4, scan, false);
    dv.setUint8(6, flags);
    ws.send(buf);
  }

  function releaseAllPressedKeys() {
    for (const { vk, flags } of pressedVKs.values()) {
      sendKey(KEY_UP, vk, 0, flags);
    }
    pressedVKs.clear();
  }

  window.addEventListener('blur', () => {
    if (activeSession) releaseAllPressedKeys();
  });

  window.addEventListener('keydown', (e) => {
    if (!activeSession) return;
    if (document.activeElement === qualitySelect) return;

    // Printable character without Ctrl/Alt/Meta -> send exact Unicode char
    // so accents & layout-specific symbols (ñ, á, €, @, ¿) arrive identically.
    if (e.key && e.key.length === 1 && !e.ctrlKey && !e.altKey && !e.metaKey) {
      e.preventDefault();
      for (let i = 0; i < e.key.length; i++) {
        sendKey(KEY_UNICODE, 0, e.key.charCodeAt(i), 0);
      }
      return;
    }

    const mapped = mapCodeToVK(e.code, e.keyCode);
    if (!mapped) return;
    e.preventDefault();
    pressedVKs.set(e.code, mapped);
    sendKey(KEY_DOWN, mapped.vk, 0, mapped.flags);
  });

  window.addEventListener('keyup', (e) => {
    if (!activeSession) return;
    const held = pressedVKs.get(e.code);
    if (!held) return;
    e.preventDefault();
    pressedVKs.delete(e.code);
    sendKey(KEY_UP, held.vk, 0, held.flags);
  });

  const CODE_TO_VK = {
    Backspace: [0x08, 0],
    Tab: [0x09, 0],
    Enter: [0x0D, 0],
    NumpadEnter: [0x0D, KEY_FLAG_EXTENDED],
    ShiftLeft: [0xA0, 0],
    ShiftRight: [0xA1, KEY_FLAG_EXTENDED],
    ControlLeft: [0xA2, 0],
    ControlRight: [0xA3, KEY_FLAG_EXTENDED],
    AltLeft: [0xA4, 0],
    AltRight: [0xA5, KEY_FLAG_EXTENDED],
    Pause: [0x13, 0],
    CapsLock: [0x14, 0],
    Escape: [0x1B, 0],
    Space: [0x20, 0],
    PageUp: [0x21, KEY_FLAG_EXTENDED],
    PageDown: [0x22, KEY_FLAG_EXTENDED],
    End: [0x23, KEY_FLAG_EXTENDED],
    Home: [0x24, KEY_FLAG_EXTENDED],
    ArrowLeft: [0x25, KEY_FLAG_EXTENDED],
    ArrowUp: [0x26, KEY_FLAG_EXTENDED],
    ArrowRight: [0x27, KEY_FLAG_EXTENDED],
    ArrowDown: [0x28, KEY_FLAG_EXTENDED],
    PrintScreen: [0x2C, KEY_FLAG_EXTENDED],
    Insert: [0x2D, KEY_FLAG_EXTENDED],
    Delete: [0x2E, KEY_FLAG_EXTENDED],
    MetaLeft: [0x5B, KEY_FLAG_EXTENDED],
    MetaRight: [0x5C, KEY_FLAG_EXTENDED],
    ContextMenu: [0x5D, KEY_FLAG_EXTENDED],
    NumLock: [0x90, KEY_FLAG_EXTENDED],
    ScrollLock: [0x91, 0],
    NumpadMultiply: [0x6A, 0],
    NumpadAdd: [0x6B, 0],
    NumpadSubtract: [0x6D, 0],
    NumpadDecimal: [0x6E, 0],
    NumpadDivide: [0x6F, KEY_FLAG_EXTENDED],
    Semicolon: [0xBA, 0],
    Equal: [0xBB, 0],
    Comma: [0xBC, 0],
    Minus: [0xBD, 0],
    Period: [0xBE, 0],
    Slash: [0xBF, 0],
    Backquote: [0xC0, 0],
    BracketLeft: [0xDB, 0],
    Backslash: [0xDC, 0],
    BracketRight: [0xDD, 0],
    Quote: [0xDE, 0],
  };

  function mapCodeToVK(code, fallbackKeyCode) {
    if (CODE_TO_VK[code]) {
      return { vk: CODE_TO_VK[code][0], flags: CODE_TO_VK[code][1] };
    }
    if (code && code.startsWith('Key') && code.length === 4) {
      return { vk: code.charCodeAt(3), flags: 0 };
    }
    if (code && code.startsWith('Digit') && code.length === 6) {
      return { vk: code.charCodeAt(5), flags: 0 };
    }
    if (code && code.startsWith('Numpad') && code.length === 7) {
      const d = code.charCodeAt(6) - 48;
      if (d >= 0 && d <= 9) return { vk: 0x60 + d, flags: 0 };
    }
    if (code && /^F([1-9]|1[0-2])$/.test(code)) {
      const n = parseInt(code.slice(1), 10);
      return { vk: 0x6F + n, flags: 0 };
    }
    if (fallbackKeyCode > 0 && fallbackKeyCode < 255) {
      return { vk: fallbackKeyCode, flags: 0 };
    }
    return null;
  }

  // --- Client Generator Modal ---
  btnOpenGen.addEventListener('click', () => {
    genError.classList.add('hidden');
    genModal.classList.remove('hidden');
    genServerAddr.focus();
  });

  btnCloseGen.addEventListener('click', () => {
    genModal.classList.add('hidden');
  });

  genModal.addEventListener('click', (e) => {
    if (e.target === genModal) genModal.classList.add('hidden');
  });

  btnDownloadClient.addEventListener('click', async () => {
    const addr = genServerAddr.value.trim();
    if (!addr) {
      genError.textContent = 'Introduce la dirección IP y puerto del servidor.';
      genError.classList.remove('hidden');
      return;
    }
    genError.classList.add('hidden');
    btnDownloadClient.disabled = true;
    btnDownloadClient.textContent = 'Generando...';

    try {
      const res = await fetch('/api/generate-client', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ server_addr: addr }),
      });
      if (!res.ok) {
        const text = await res.text();
        genError.textContent = text.trim() || 'No se pudo generar el cliente.';
        genError.classList.remove('hidden');
        return;
      }

      const blob = await res.blob();
      const url = URL.createObjectURL(blob);
      const a = document.createElement('a');
      a.href = url;
      a.download = 'ClienteRemoto.exe';
      document.body.appendChild(a);
      a.click();
      a.remove();
      setTimeout(() => URL.revokeObjectURL(url), 5000);

      genModal.classList.add('hidden');
      showNotice(`ClienteRemoto.exe generado para ${addr}. Puedes copiarlo en cualquier equipo.`);
    } catch {
      genError.textContent = 'Error de conexión con el servidor local.';
      genError.classList.remove('hidden');
    } finally {
      btnDownloadClient.disabled = false;
      btnDownloadClient.textContent = 'Descargar ClienteRemoto.exe';
    }
  });

  // Init
  loadServerInfo();
  connectWS();
})();
