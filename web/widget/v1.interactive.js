/*! epocha widget interactive primitives v1 — 实验型交互题型渲染器（ADR-030）
 *
 * 为什么单独成文件：v1.js 核心必须守住 gzip <30KB（DESIGN §13、ADR-025 第 5 条）。
 * 这些渲染器只在服务端下发 masks/path/order/select/sequence/placements 题型时按需加载，
 * 与核心共用同一个 Widget 状态机；模块本身也受体积守护。它不包含答案、目标索引或判分逻辑：
 * 只采集用户操作并写成 canonical answer，唯一的通过点仍是服务端 /api/redeem。
 */
(function (global) {
  "use strict";
  var api = global.EpoCHA;
  if (!api || typeof api._installInteractive !== "function") return;
  api._installInteractive(function (h) {
    var Widget = h.Widget, STATES = h.STATES, el = h.el, tr = h.tr,
        intField = h.intField, window = h.global, clear = h.clear,
        validGrid = h.validGrid, validPrompts = h.validPrompts;

  function hexID(s) {
    return typeof s === "string" && /^[0-9a-fA-F]{4}$/.test(s);
  }

  function validRotate(q, iw, ih) {
    return validGrid(q, iw, ih) && q.input_mode === "rotate" &&
      typeof q.initial_masks === "string" && /^[0-9a-fA-F]{9}$/.test(q.initial_masks);
  }

  function validPathPayload(q, iw, ih) {
    if (!validGrid(q, iw, ih) || q.input_mode !== "path") return false;
    var n = intField(q, "target_len");
    if (n === null || n < 6 || n > 8) return false;
    if (intField(q, "target_y") === null || intField(q, "target_h") === null) return false;
    if (!Array.isArray(q.cell_ids) || q.cell_ids.length !== 36) return false;
    var seen = {};
    for (var i = 0; i < q.cell_ids.length; i++) {
      if (!hexID(q.cell_ids[i]) || seen[q.cell_ids[i]]) return false;
      seen[q.cell_ids[i]] = true;
    }
    return true;
  }

  function validRank(q, iw, ih) {
    if (q.input_mode !== "rank" || !validPrompts(q)) return false;
    if (!Array.isArray(q.cards) || q.cards.length !== 5) return false;
    var seen = {};
    for (var i = 0; i < q.cards.length; i++) {
      var c = q.cards[i];
      if (!c || typeof c.id !== "string" || !/^[A-E]$/.test(c.id) || seen[c.id]) return false;
      seen[c.id] = true;
      if (intField(c, "x") === null || intField(c, "y") === null ||
          intField(c, "w") === null || intField(c, "h") === null) return false;
      if (c.x < 0 || c.y < 0 || c.w < 1 || c.h < 1 || c.x + c.w > iw || c.y + c.h > ih) return false;
    }
    return true;
  }

  function validSelectPayload(q, iw, ih) {
    if (!validGrid(q, iw, ih) || q.input_mode !== "select") return false;
    var sx = intField(q, "sample_x"), sy = intField(q, "sample_y");
    var sw = intField(q, "sample_w"), sh = intField(q, "sample_h");
    if (sx === null || sy === null || sw === null || sh === null) return false;
    if (sx < 0 || sy < 0 || sw < 1 || sh < 1 || sx + sw > iw || sy + sh > ih) return false;
    if (!Array.isArray(q.cell_ids) || q.cell_ids.length !== 36) return false;
    var seen = {};
    for (var i = 0; i < q.cell_ids.length; i++) {
      if (!hexID(q.cell_ids[i]) || seen[q.cell_ids[i]]) return false;
      seen[q.cell_ids[i]] = true;
    }
    return true;
  }

  function validProgressivePayload(q, iw, ih) {
    if (q.input_mode !== "progressive" || !validPrompts(q)) return false;
    if (intField(q, "round_count") !== 3) return false;
    var cols = intField(q, "grid_cols"), rows = intField(q, "grid_rows");
    var cell = intField(q, "grid_cell"), gap = intField(q, "grid_gap");
    var gy = intField(q, "grid_y"), ry = intField(q, "round_y");
    var rh = intField(q, "round_h"), gx = intField(q, "grid_x");
    var sx = intField(q, "sample_x"), sy = intField(q, "sample_y");
    var sw = intField(q, "sample_w"), sh = intField(q, "sample_h");
    if (cols !== 3 || rows !== 3 || cell === null || gap === null ||
        gy === null || ry === null || rh === null || gx === null ||
        sx === null || sy === null || sw === null || sh === null) return false;
    var gw = cols * cell + (cols - 1) * gap;
    var gh = rows * cell + (rows - 1) * gap;
    if (ry < 0 || rh < 1 || ry + 3 * rh > ih) return false;
    if (gx < 0 || gy < 0 || gx + gw > iw || gy + gh > rh) return false;
    if (sx < 0 || sy < 0 || sw < 1 || sh < 1 || sx + sw > iw || sy + sh > rh) return false;
    return true;
  }

  function validShellPayload(q, iw, ih) {
    if (q.input_mode !== "shell" || q.render_mode !== "sprite_sheet" || !validPrompts(q)) return false;
    var rounds = intField(q, "rounds"), per = intField(q, "frames_per_round");
    var count = intField(q, "frame_count");
    if (rounds !== 3 || per === null || per < 2 || count !== rounds * per) return false;
    if (!Array.isArray(q.boxes) || q.boxes.length !== 4) return false;
    for (var i = 0; i < q.boxes.length; i++) {
      var b = q.boxes[i];
      if (!b || intField(b, "x") === null || intField(b, "y") === null ||
          intField(b, "w") === null || intField(b, "h") === null) return false;
      if (b.x < 0 || b.y < 0 || b.w < 44 || b.h < 44 || b.x + b.w > iw || b.y + b.h > ih) return false;
    }
    return true;
  }

  function validPlacementsPayload(q, iw, ih) {
    if (q.input_mode !== "placements" || !validPrompts(q)) return false;
    if (!Array.isArray(q.pieces) || q.pieces.length !== 5 ||
        !Array.isArray(q.slots) || q.slots.length !== 5) return false;
    var seen = {};
    for (var i = 0; i < 5; i++) {
      var p = q.pieces[i];
      if (!p || typeof p.id !== "string" || !/^[0-4]$/.test(p.id) || seen[p.id]) return false;
      seen[p.id] = true;
      if (intField(p, "x") === null || intField(p, "y") === null ||
          intField(p, "w") === null || intField(p, "h") === null) return false;
      if (p.x < 0 || p.y < 0 || p.w < 44 || p.h < 44 || p.x + p.w > iw || p.y + p.h > ih) return false;
      var s = q.slots[i];
      if (!s || intField(s, "x") === null || intField(s, "y") === null ||
          intField(s, "w") === null || intField(s, "h") === null) return false;
      if (s.x < 0 || s.y < 0 || s.w < 44 || s.h < 44 || s.x + s.w > iw || s.y + s.h > ih) return false;
    }
    return true;
  }

    var I18N = h.i18n;
    var ZH = {
      noteRotate: "点击或按 Enter 将路径块顺时针旋转 90°；Shift+Enter 可逆时针。全部连成一张网络后提交。每题一次作答机会。",
      notePath: "按顶部符号顺序，点击或拖动连接相邻格子；Backspace 撤销上一步。每题一次作答机会。",
      noteRank: "按线段数量从少到多依次选择五张卡；已选卡不会重复。每题一次作答机会。",
      noteSelect: "点击与顶部样本完全相同的角色；提交前可以改选。每题一次作答机会。",
      noteProgressive: "三轮依次进行：每轮点击与顶部样本完全相同的图形。选择后客户端不判断对错。每题一次作答机会。",
      noteShell: "每轮先记住目标所在容器，再跟踪交换；动画结束后点击目标容器。三轮答案一起提交。每题一次作答机会。",
      notePlacements: "选择部件，用“旋转”调整方向，再点击匹配的槽位；槽位有缺口轮廓，五个部件都要放好。每题一次作答机会。",
      roundLabel: "第 {n}/{total} 轮",
      tileLabel: "第 {n} 个路径块",
      tileTurned: "第 {n} 个路径块旋转了 {deg}°",
      rotateReset: "已重置全部旋转。",
      pathStep: "已连接 {n} 格",
      pathUndo: "已撤销最后一步。",
      errIncompleteRotate: "请至少旋转一个路径块后再提交。",
      errIncompletePath: "请连接完整的目标符号序列。",
      errIncompleteRank: "请选满五张卡片。",
      errIncompleteSelect: "请选择一个角色。",
      errIncompleteProgressive: "请完成三轮选择后再提交。",
      errIncompleteShell: "请完成三轮容器选择后再提交。",
      errIncompletePlacements: "请把五个部件都放进槽位后再提交。",
      rankOrder: "已选顺序：{order}",
      rankRemoved: "已取消第 {n} 张卡。",
      selectChosen: "已选择第 {n} 个角色。",
      progressiveChosen: "第 {n} 轮已选择。",
      shellWatch: "观看第 {n}/{total} 轮，动画结束前不能选择。",
      shellChoose: "动画结束：点击目标容器。",
      shellReduced: "当前系统开启了“减少动态效果”，本题需要动画；请更换其他验证。",
      pieceLabel: "部件 {id}",
      slotLabel: "槽位 {n}",
      placementsProgress: "已放置 {n}/5",
      pieceSelected: "已选择部件 {id}，可旋转后放入槽位。",
      pieceRotated: "部件方向 {deg}°。",
      localReset: "已清空本题的本地操作，题目仍然有效。",
      imageAltRotate: "验证图片：旋转路径块使它们连成网络",
      imageAltPath: "验证图片：按目标符号序列连接格子",
      imageAltRank: "验证图片：按线段数量排列五张卡",
      imageAltSelect: "验证图片：找出与顶部样本相同的角色",
      imageAltProgressive: "验证图片：三轮中分别找出与顶部样本相同的图形",
      imageAltShell: "验证图片：跟踪隐藏目标的容器交换",
      imageAltPlacements: "验证图片：把五个部件旋转后放入匹配的槽位",
    };
    var EN = {
      noteRotate: "Click or press Enter to rotate a tile 90° clockwise; Shift+Enter rotates counter-clockwise. Submit once all paths form one network. One attempt per question.",
      notePath: "Follow the target symbols at the top: click or drag through adjacent cells; Backspace undoes the last step. One attempt per question.",
      noteRank: "Select the five cards from the fewest to the most line segments; a card cannot be selected twice. One attempt per question.",
      noteSelect: "Click the character that exactly matches the sample at the top; you can change it before submitting. One attempt per question.",
      noteProgressive: "Three rounds in order: click the shape that exactly matches each round's top sample. The client does not judge a round before the final submit. One attempt per question.",
      noteShell: "Remember which container holds the target, track the swaps, then click the target container after each round. All three answers are submitted together. One attempt per question.",
      notePlacements: "Select a piece, use Rotate to change its orientation, then click the matching slot. Slot outlines have a notched texture; place all five pieces. One attempt per question.",
      roundLabel: "Round {n}/{total}",
      tileLabel: "Tile {n}",
      tileTurned: "Tile {n} rotated {deg}°",
      rotateReset: "All rotations reset.",
      pathStep: "{n} cells connected",
      pathUndo: "Last step undone.",
      errIncompleteRotate: "Rotate at least one tile before submitting.",
      errIncompletePath: "Connect the full target sequence.",
      errIncompleteRank: "Select all five cards.",
      errIncompleteSelect: "Choose one character.",
      errIncompleteProgressive: "Complete all three rounds before submitting.",
      errIncompleteShell: "Choose a container in all three rounds before submitting.",
      errIncompletePlacements: "Place all five pieces before submitting.",
      rankOrder: "Selected order: {order}",
      rankRemoved: "Card {n} deselected.",
      selectChosen: "Character {n} selected.",
      progressiveChosen: "Round {n} selected.",
      shellWatch: "Watch round {n}/{total}; selection is disabled until the animation ends.",
      shellChoose: "Animation complete: click the target container.",
      shellReduced: "Reduced motion is enabled and this question needs animation; please switch to another verification.",
      pieceLabel: "Piece {id}",
      slotLabel: "Slot {n}",
      placementsProgress: "Placed {n}/5",
      pieceSelected: "Piece {id} selected; rotate it and place it in a slot.",
      pieceRotated: "Piece orientation {deg}°.",
      localReset: "Local input cleared; this question is still valid.",
      imageAltRotate: "Verification image: rotate path tiles to form a network",
      imageAltPath: "Verification image: trace the target symbol sequence",
      imageAltRank: "Verification image: order five cards by line-segment count",
      imageAltSelect: "Verification image: find the character matching the sample",
      imageAltProgressive: "Verification image: find the matching shape in each of three rounds",
      imageAltShell: "Verification image: track the target through container swaps",
      imageAltPlacements: "Verification image: rotate five pieces into matching slots",
    };
    for (var ik in ZH) I18N["zh-CN"][ik] = ZH[ik];
    for (var jk in EN) I18N.en[jk] = EN[jk];

    Widget._validInteractive = function (at, q, iw, ih) {
      if (at === "masks") return validRotate(q, iw, ih);
      if (at === "path") return validPathPayload(q, iw, ih);
      if (at === "order") return validRank(q, iw, ih);
      if (at === "select") return validSelectPayload(q, iw, ih);
      if (at === "placements") return validPlacementsPayload(q, iw, ih);
      if (at === "sequence") {
        if (q.input_mode === "shell") return validShellPayload(q, iw, ih);
        if (q.input_mode === "progressive") return validProgressivePayload(q, iw, ih);
        return false;
      }
      return false;
    };
  // --- 2026-11-12 新增交互原语（ADR-030）---
  // 这些控件只负责采集用户操作并写成 canonical answer；不判断对错，不生成 token。

  function hexVal(c) {
    if (c >= "0" && c <= "9") return c.charCodeAt(0) - 48;
    if (c >= "a" && c <= "f") return c.charCodeAt(0) - 87;
    if (c >= "A" && c <= "F") return c.charCodeAt(0) - 55;
    return 0;
  }

  function rotateMaskCW(m) {
    m &= 0x0f;
    return ((m << 1) & 0x0f) | ((m >> 3) & 0x01);
  }

  function finalMask(initial, turns) {
    var m = initial;
    for (var i = 0; i < (turns & 3); i++) m = rotateMaskCW(m);
    return m;
  }

  Widget.prototype._sliceBackground = function (node, q, iw, ih, x, y, w, h) {
    node.style.backgroundImage = "url(data:" + q.image_mime + ";base64," + q.image_base64 + ")";
    node.style.backgroundSize = (iw / w * 100) + "% " + (ih / h * 100) + "%";
    // background-position 的百分比公式是 (容器−图片)×p；图片比容器大时 p 为正才把
    // 目标裁剪区移到容器里（写成负号会把裁剪区推到反方向）。
    var px = iw > w ? (x / (iw - w) * 100) : 0;
    var py = ih > h ? (y / (ih - h) * 100) : 0;
    node.style.backgroundPosition = px + "% " + py + "%";
  };


  Widget.prototype._gridPoint = function (q, idx) {
    var cols = intField(q, "grid_cols"), x = intField(q, "grid_x"), y = intField(q, "grid_y");
    var cell = intField(q, "grid_cell"), gap = intField(q, "grid_gap");
    var r = Math.floor(idx / cols), c = idx % cols;
    return {
      x: x + c * (cell + gap) + cell / 2,
      y: y + r * (cell + gap) + cell / 2,
      left: x + c * (cell + gap),
      top: y + r * (cell + gap),
      cell: cell
    };
  };


  Widget.prototype._placeGridButton = function (q, iw, ih, idx, label) {
    var p = this._gridPoint(q, idx);
    return el("button", {
      type: "button", class: "cell", "data-cell": String(idx),
      "aria-label": label,
      "aria-describedby": this.note.getAttribute("id") + " " + this.status.getAttribute("id"),
      style: "left:" + (p.left / iw * 100) + "%;top:" + (p.top / ih * 100) + "%;width:" +
        (p.cell / iw * 100) + "%;height:" + (p.cell / ih * 100) + "%"
    });
  };


  Widget.prototype._syncPathLine = function (q, iw, ih) {
    var pts = [];
    for (var i = 0; i < this.pathCells.length; i++) {
      var p = this._gridPoint(q, this.pathCells[i]);
      pts.push(p.x + "," + p.y);
    }
    this.pathLine.setAttribute("points", pts.join(" "));
    this.pathLayer.setAttribute("viewBox", "0 0 " + iw + " " + ih);
  };


  Widget.prototype._syncPathAnswer = function (q, iw, ih) {
    var parts = [];
    for (var i = 0; i < this.pathCells.length; i++) {
      var n = this.pathCells[i];
      parts.push((n < 10 ? "0" : "") + String(n));
    }
    this.answer.value = this.pathCells.length === this.pathTargetLen ? "v1:" + parts.join(".") : "";
    this._syncPathLine(q, iw, ih);
    for (var k = 0; k < this.cellButtons.length; k++) {
      var btn = this.cellButtons[k];
      var pos = this.pathCells.indexOf(k);
      btn.setAttribute("data-path", pos >= 0 ? "true" : "false");
      var badge = btn.firstChild;
      if (badge) {
        badge.textContent = pos >= 0 ? String(pos + 1) : "";
        badge.setAttribute("class", pos >= 0 ? "pathnum" : "pathnum hidden");
      }
    }
    this.cellStatus.textContent = tr(this.o.lang, "pathStep", { n: this.pathCells.length });
    this._updateSubmit();
  };


  Widget.prototype._pathTap = function (idx) {
    if (this.destroyed || this.state !== STATES.READY || this.answerMode !== "path") return;
    var path = this.pathCells;
    if (path.length && idx === path[path.length - 1]) return;
    if (path.length >= 2 && idx === path[path.length - 2]) {
      path.pop();
      this._syncPathAnswer(this.pathChallenge, this.pathIW, this.pathIH);
      return;
    }
    if (path.indexOf(idx) >= 0) return;
    if (path.length === 0) {
      path.push(idx);
    } else {
      var last = path[path.length - 1];
      var lr = Math.floor(last / this.gridCols), lc = last % this.gridCols;
      var nr = Math.floor(idx / this.gridCols), nc = idx % this.gridCols;
      if (Math.abs(lr - nr) + Math.abs(lc - nc) !== 1) return;
      if (path.length >= this.pathTargetLen) return;
      path.push(idx);
    }
    this._syncPathAnswer(this.pathChallenge, this.pathIW, this.pathIH);
  };


  Widget.prototype._pathUndo = function () {
    if (this.answerMode !== "path" || !this.pathCells.length) return;
    this.pathCells.pop();
    this._syncPathAnswer(this.pathChallenge, this.pathIW, this.pathIH);
    this.cellStatus.textContent = tr(this.o.lang, "pathUndo");
  };


  Widget.prototype._pathKey = function (ev, idx) {
    if (this.state !== STATES.READY || this.answerMode !== "path") return;
    var cols = this.gridCols, rows = Math.floor(this.cellButtons.length / cols);
    var r = Math.floor(idx / cols), c = idx % cols, ni = -1;
    if (ev.key === "Enter" || ev.key === " ") { ev.preventDefault(); this._pathTap(idx); return; }
    if (ev.key === "Backspace") { ev.preventDefault(); this._pathUndo(); return; }
    if (ev.key === "ArrowLeft" && c > 0) ni = idx - 1;
    else if (ev.key === "ArrowRight" && c + 1 < cols) ni = idx + 1;
    else if (ev.key === "ArrowUp" && r > 0) ni = idx - cols;
    else if (ev.key === "ArrowDown" && r + 1 < rows) ni = idx + cols;
    if (ni >= 0 && this.cellButtons[ni]) { ev.preventDefault(); this.cellButtons[ni].focus(); }
  };


  Widget.prototype._renderRotate = function (q, iw, ih) {
    this._clearCells();
    this.answerMode = "rotate";
    this.answer.value = "";
    this.answer.setAttribute("class", "ans hidden");
    this.pairWrap.setAttribute("class", "pairs hidden");
    this.angleWrap.setAttribute("class", "angle hidden");
    this.timingWrap.setAttribute("class", "timing hidden");
    this.optionsWrap.setAttribute("class", "options hidden");
    this.cellWrap.setAttribute("class", "cells");
    this.cellStatus.setAttribute("class", "cellsum");
    this.auxWrap.setAttribute("class", "aux");
    this.btnUndo.setAttribute("class", "hidden");
    this.btnReset.setAttribute("class", "");
    var raw = String(q.initial_masks || "");
    this.gridCols = intField(q, "grid_cols");
    var self = this;
    for (var i = 0; i < raw.length; i++) {
      this.tileMasks.push(hexVal(raw.charAt(i)));
      this.tileTurns.push(0);
      this.tileBusyUntil.push(0);
      var idx = i;
      var btn = this._placeGridButton(q, iw, ih, idx, tr(this.o.lang, "tileLabel", { n: idx + 1 }));
      var p = this._gridPoint(q, idx);
      var tile = el("span", { class: "tileimg" });
      this._sliceBackground(tile, q, iw, ih, p.left, p.top, p.cell, p.cell);
      tile.style.transform = "rotate(0deg)";
      btn.appendChild(tile);
      var fn = (function (cellIdx) {
        return function (e) { e.preventDefault(); self._rotateTile(cellIdx, 1); };
      })(idx);
      var kf = (function (cellIdx) {
        return function (e) {
          if (e.key === "Enter" || e.key === " ") {
            e.preventDefault();
            self._rotateTile(cellIdx, e.shiftKey ? -1 : 1);
          }
        };
      })(idx);
      btn._epochaClick = fn;
      btn._epochaKey = kf;
      this._on(btn, "click", fn);
      this._on(btn, "keydown", kf);
      this.cellButtons.push(btn);
      this.cellWrap.appendChild(btn);
    }
    this.cellStatus.textContent = tr(this.o.lang, "selectedCount", { n: 0 });
  };


  Widget.prototype._rotateTile = function (idx, dir) {
    if (this.destroyed || this.state !== STATES.READY || this.answerMode !== "rotate") return;
    var now = Date.now();
    if (this.tileBusyUntil[idx] && now < this.tileBusyUntil[idx]) return;
    this.tileBusyUntil[idx] = now + 150;
    this.tileTurns[idx] = (this.tileTurns[idx] + dir + 4) % 4;
    var btn = this.cellButtons[idx];
    var tile = btn && btn.firstChild;
    if (tile) tile.style.transform = "rotate(" + (this.tileTurns[idx] * 90) + "deg)";
    var hex = "";
    var any = false;
    for (var i = 0; i < this.tileMasks.length; i++) {
      hex += finalMask(this.tileMasks[i], this.tileTurns[i]).toString(16);
      if (this.tileTurns[i]) any = true;
    }
    this.answer.value = "v1:" + hex;
    this.cellStatus.textContent = tr(this.o.lang, "tileTurned", {
      n: idx + 1, deg: this.tileTurns[idx] * 90
    });
    this._updateSubmit();
  };


  Widget.prototype._renderPath = function (q, iw, ih) {
    this._clearCells();
    this.answerMode = "path";
    this.answer.value = "";
    this.answer.setAttribute("class", "ans hidden");
    this.pairWrap.setAttribute("class", "pairs hidden");
    this.angleWrap.setAttribute("class", "angle hidden");
    this.timingWrap.setAttribute("class", "timing hidden");
    this.optionsWrap.setAttribute("class", "options hidden");
    this.cellWrap.setAttribute("class", "cells");
    this.cellStatus.setAttribute("class", "cellsum");
    this.auxWrap.setAttribute("class", "aux");
    this.btnUndo.setAttribute("class", "");
    this.btnReset.setAttribute("class", "");
    this.pathLayer.setAttribute("class", "pathlayer");
    this.pathLayer.setAttribute("viewBox", "0 0 " + iw + " " + ih);
    this.pathChallenge = q;
    this.pathIW = iw;
    this.pathIH = ih;
    this.gridCols = intField(q, "grid_cols");
    this.pathTargetLen = intField(q, "target_len");
    var self = this;
    var ids = Array.isArray(q.cell_ids) ? q.cell_ids : [];
    for (var i = 0; i < this.gridCols * intField(q, "grid_rows"); i++) {
      var idx = i;
      var label = tr(this.o.lang, "cellLabel", { n: idx + 1 });
      var btn = this._placeGridButton(q, iw, ih, idx, label + (ids[idx] ? " (" + ids[idx] + ")" : ""));
      var badge = el("span", { class: "pathnum hidden" }, "");
      btn.appendChild(badge);
      var fn = (function (cellIdx) {
        return function (e) { e.preventDefault(); self._pathTap(cellIdx); };
      })(idx);
      var kf = (function (cellIdx) {
        return function (e) { self._pathKey(e, cellIdx); };
      })(idx);
      var df = (function (cellIdx) {
        return function (e) { if (e.button === 0) { self.pathDragging = true; self._pathTap(cellIdx); } };
      })(idx);
      var ef = (function (cellIdx) {
        return function () { if (self.pathDragging) { self._pathTap(cellIdx); } };
      })(idx);
      btn._epochaClick = fn;
      btn._epochaKey = kf;
      btn._epochaDown = df;
      btn._epochaEnter = ef;
      this._on(btn, "click", fn);
      this._on(btn, "keydown", kf);
      this._on(btn, "pointerdown", df);
      this._on(btn, "pointerenter", ef);
      this.cellButtons.push(btn);
      this.cellWrap.appendChild(btn);
    }
    this._syncPathAnswer(q, iw, ih);
  };


  Widget.prototype._renderRank = function (q, iw, ih) {
    this._clearCells();
    this.answerMode = "rank";
    this.answer.value = "";
    this.answer.setAttribute("class", "ans hidden");
    this.pairWrap.setAttribute("class", "pairs hidden");
    this.angleWrap.setAttribute("class", "angle hidden");
    this.timingWrap.setAttribute("class", "timing hidden");
    this.optionsWrap.setAttribute("class", "options hidden");
    this.cellWrap.setAttribute("class", "cells");
    this.cellStatus.setAttribute("class", "cellsum");
    this.auxWrap.setAttribute("class", "aux");
    this.orderBar.setAttribute("class", "ordersum");
    this.btnUndo.setAttribute("class", "");
    this.btnReset.setAttribute("class", "");
    this.rankCards = q.cards;
    var self = this;
    for (var i = 0; i < q.cards.length; i++) {
      var card = q.cards[i];
      var btn = el("button", {
        type: "button", class: "cell", "data-card": String(card.id),
        "aria-label": tr(this.o.lang, "cellLabel", { n: i + 1 }),
        "aria-describedby": this.note.getAttribute("id") + " " + this.status.getAttribute("id"),
        style: "left:" + (card.x / iw * 100) + "%;top:" + (card.y / ih * 100) + "%;width:" +
          (card.w / iw * 100) + "%;height:" + (card.h / ih * 100) + "%"
      });
      var badge = el("span", { class: "rankno hidden" }, "");
      btn.appendChild(badge);
      var fn = (function (cardID) {
        return function (e) { e.preventDefault(); self._rankToggle(cardID); };
      })(card.id);
      btn._epochaClick = fn;
      this._on(btn, "click", fn);
      this.cellButtons.push(btn);
      this.cellWrap.appendChild(btn);
    }
    this._syncRank();
  };


  Widget.prototype._syncRank = function () {
    var parts = [];
    for (var i = 0; i < this.rankOrder.length; i++) {
      var id = this.rankOrder[i];
      parts.push(id);
      for (var k = 0; k < this.cellButtons.length; k++) {
        if (this.cellButtons[k].getAttribute("data-card") === id) {
          var badge = this.cellButtons[k].firstChild;
          badge.textContent = String(i + 1);
          badge.setAttribute("class", "rankno");
          this.cellButtons[k].setAttribute("data-sel", "true");
        }
      }
    }
    this.answer.value = this.rankOrder.length === 5 ? "v1:" + parts.join(".") : "";
    this.orderBar.textContent = this.rankOrder.length
      ? tr(this.o.lang, "rankOrder", { order: parts.join(" › ") })
      : tr(this.o.lang, "selectedCount", { n: 0 });
    this._updateSubmit();
  };


  Widget.prototype._rankToggle = function (id) {
    if (this.destroyed || this.state !== STATES.READY || this.answerMode !== "rank") return;
    var pos = this.rankOrder.indexOf(id);
    if (pos >= 0) {
      this.rankOrder.splice(pos, 1);
      this.cellStatus.textContent = tr(this.o.lang, "rankRemoved", { n: pos + 1 });
    } else if (this.rankOrder.length < 5) {
      this.rankOrder.push(id);
      this.cellStatus.textContent = tr(this.o.lang, "selectedCount", { n: this.rankOrder.length });
    }
    for (var k = 0; k < this.cellButtons.length; k++) {
      this.cellButtons[k].setAttribute("data-sel", "false");
      var badge = this.cellButtons[k].firstChild;
      badge.textContent = "";
      badge.setAttribute("class", "rankno hidden");
    }
    this._syncRank();
  };


  Widget.prototype._renderSelect = function (q, iw, ih) {
    this._clearCells();
    this.answerMode = "select";
    this.answer.value = "";
    this.answer.setAttribute("class", "ans hidden");
    this.pairWrap.setAttribute("class", "pairs hidden");
    this.angleWrap.setAttribute("class", "angle hidden");
    this.timingWrap.setAttribute("class", "timing hidden");
    this.optionsWrap.setAttribute("class", "options hidden");
    this.cellWrap.setAttribute("class", "cells");
    this.cellStatus.setAttribute("class", "cellsum");
    this.auxWrap.setAttribute("class", "aux");
    this.btnUndo.setAttribute("class", "hidden");
    this.btnReset.setAttribute("class", "");
    this.gridCols = intField(q, "grid_cols");
    var self = this;
    var ids = Array.isArray(q.cell_ids) ? q.cell_ids : [];
    for (var i = 0; i < this.gridCols * intField(q, "grid_rows"); i++) {
      var idx = i;
      var btn = this._placeGridButton(q, iw, ih, idx,
        tr(this.o.lang, "cellLabel", { n: idx + 1 }) + (ids[idx] ? " (" + ids[idx] + ")" : ""));
      var fn = (function (cellIdx) {
        return function (e) { e.preventDefault(); self._selectCell(cellIdx); };
      })(idx);
      var kf = (function (cellIdx) {
        return function (e) {
          var cols = self.gridCols, rows = Math.floor(self.cellButtons.length / cols);
          var r = Math.floor(cellIdx / cols), c = cellIdx % cols, ni = -1;
          if (e.key === "ArrowLeft" && c > 0) ni = cellIdx - 1;
          else if (e.key === "ArrowRight" && c + 1 < cols) ni = cellIdx + 1;
          else if (e.key === "ArrowUp" && r > 0) ni = cellIdx - cols;
          else if (e.key === "ArrowDown" && r + 1 < rows) ni = cellIdx + cols;
          if (ni >= 0 && self.cellButtons[ni]) { e.preventDefault(); self.cellButtons[ni].focus(); }
        };
      })(idx);
      btn._epochaClick = fn;
      btn._epochaKey = kf;
      this._on(btn, "click", fn);
      this._on(btn, "keydown", kf);
      this.cellButtons.push(btn);
      this.cellWrap.appendChild(btn);
    }
    this.cellStatus.textContent = tr(this.o.lang, "selectedCount", { n: 0 });
  };


  Widget.prototype._selectCell = function (idx) {
    if (this.destroyed || this.state !== STATES.READY || this.answerMode !== "select") return;
    this.selectIndex = idx;
    for (var i = 0; i < this.cellButtons.length; i++) {
      this.cellButtons[i].setAttribute("data-sel", i === idx ? "true" : "false");
    }
    this.answer.value = "v1:" + String(idx);
    this.cellStatus.textContent = tr(this.o.lang, "selectChosen", { n: idx + 1 });
    this._updateSubmit();
  };


  Widget.prototype._removeCellButtons = function () {
    for (var i = 0; i < this.cellButtons.length; i++) {
      var b = this.cellButtons[i];
      if (b._epochaClick) this._off(b, "click", b._epochaClick);
      if (b._epochaKey) this._off(b, "keydown", b._epochaKey);
      if (b._epochaDown) this._off(b, "pointerdown", b._epochaDown);
      if (b._epochaEnter) this._off(b, "pointerenter", b._epochaEnter);
    }
    this.cellButtons = [];
    clear(this.cellWrap);
  };

  // --- progressive-target-v1 ---


  Widget.prototype._renderProgressive = function (q, iw, ih) {
    this._clearCells();
    this.answerMode = "progressive";
    this.answer.value = "";
    this.answer.setAttribute("class", "ans hidden");
    this.pairWrap.setAttribute("class", "pairs hidden");
    this.angleWrap.setAttribute("class", "angle hidden");
    this.timingWrap.setAttribute("class", "timing hidden");
    this.optionsWrap.setAttribute("class", "options hidden");
    this.cellWrap.setAttribute("class", "cells");
    this.cellStatus.setAttribute("class", "cellsum");
    this.auxWrap.setAttribute("class", "aux");
    this.orderBar.setAttribute("class", "ordersum hidden");
    this.btnUndo.setAttribute("class", "hidden");
    this.btnReset.setAttribute("class", "");
    this.btnRotate.setAttribute("class", "hidden");
    this.progressiveChallenge = q;
    this.progressiveIW = iw;
    this.progressiveIH = ih;
    this.progressiveRound = 0;
    this.progressiveSelections = [-1, -1, -1];
    this.gridCols = intField(q, "grid_cols");
    var rounds = intField(q, "round_count");
    var ry = intField(q, "round_y"), rh = intField(q, "round_h");
    this.roundCovers = [];
    for (var r = 0; r < rounds; r++) {
      var cover = el("div", { class: "roundcover" });
      cover.style.top = ((ry + r * rh) / ih * 100) + "%";
      cover.style.height = (rh / ih * 100) + "%";
      this.imgBox.appendChild(cover);
      this.roundCovers.push(cover);
    }
    this._progressiveShowRound(0);
  };


  Widget.prototype._progressiveShowRound = function (round) {
    var q = this.progressiveChallenge, iw = this.progressiveIW, ih = this.progressiveIH;
    if (!q) return;
    this.progressiveRound = round;
    this._removeCellButtons();
    this.answer.value = "";
    var cols = intField(q, "grid_cols"), rows = intField(q, "grid_rows");
    var gx = intField(q, "grid_x"), gy = intField(q, "grid_y");
    var cell = intField(q, "grid_cell"), gap = intField(q, "grid_gap");
    var ry = intField(q, "round_y"), rh = intField(q, "round_h");
    var self = this;
    for (var idx = 0; idx < cols * rows; idx++) {
      var col = idx % cols, row = Math.floor(idx / cols);
      var x = gx + col * (cell + gap);
      var y = ry + round * rh + gy + row * (cell + gap);
      var btn = el("button", {
        type: "button", class: "cell", "data-cell": String(idx),
        "aria-label": tr(this.o.lang, "cellLabel", { n: idx + 1 }),
        "aria-describedby": this.note.getAttribute("id") + " " + this.status.getAttribute("id"),
        style: "left:" + (x / iw * 100) + "%;top:" + (y / ih * 100) + "%;width:" +
          (cell / iw * 100) + "%;height:" + (cell / ih * 100) + "%"
      });
      btn.setAttribute("aria-pressed", "false");
      var fn = (function (cellIdx) {
        return function (e) { e.preventDefault(); self._progressiveSelect(cellIdx); };
      })(idx);
      var kf = (function (cellIdx) {
        return function (e) { self._progressiveKey(e, cellIdx); };
      })(idx);
      btn._epochaClick = fn;
      btn._epochaKey = kf;
      this._on(btn, "click", fn);
      this._on(btn, "keydown", kf);
      this.cellButtons.push(btn);
      this.cellWrap.appendChild(btn);
    }
    for (var c = 0; c < this.roundCovers.length; c++) {
      this.roundCovers[c].setAttribute("class", c === round ? "roundcover hidden" : "roundcover");
    }
    this.cellStatus.textContent = tr(this.o.lang, "roundLabel", { n: round + 1, total: 3 });
    this._setInputsEnabled(this.state === STATES.READY);
    this._updateSubmit();
  };


  Widget.prototype._progressiveKey = function (ev, idx) {
    if (this.state !== STATES.READY || this.answerMode !== "progressive") return;
    var cols = intField(this.progressiveChallenge, "grid_cols");
    var rows = intField(this.progressiveChallenge, "grid_rows");
    var r = Math.floor(idx / cols), c = idx % cols, ni = -1;
    if (ev.key === "ArrowLeft" && c > 0) ni = idx - 1;
    else if (ev.key === "ArrowRight" && c + 1 < cols) ni = idx + 1;
    else if (ev.key === "ArrowUp" && r > 0) ni = idx - cols;
    else if (ev.key === "ArrowDown" && r + 1 < rows) ni = idx + cols;
    if (ni >= 0 && this.cellButtons[ni]) { ev.preventDefault(); this.cellButtons[ni].focus(); }
  };


  Widget.prototype._progressiveSelect = function (idx) {
    if (this.destroyed || this.state !== STATES.READY || this.answerMode !== "progressive") return;
    var q = this.progressiveChallenge;
    if (!q) return;
    var rounds = intField(q, "round_count");
    this.progressiveSelections[this.progressiveRound] = idx;
    if (this.progressiveRound < rounds - 1) {
      this.progressiveSelections[this.progressiveRound + 1] = -1;
      this._progressiveShowRound(this.progressiveRound + 1);
      return;
    }
    for (var i = 0; i < this.cellButtons.length; i++) {
      this.cellButtons[i].setAttribute("aria-pressed", i === idx ? "true" : "false");
      this.cellButtons[i].setAttribute("data-sel", i === idx ? "true" : "false");
    }
    var parts = [];
    for (var k = 0; k < this.progressiveSelections.length; k++) {
      parts.push(String(this.progressiveSelections[k]));
    }
    this.answer.value = "v1:" + parts.join(".");
    this.cellStatus.textContent = tr(this.o.lang, "progressiveChosen", { n: rounds });
    this._updateSubmit();
  };

  // --- shell-track-v1 ---


  Widget.prototype._renderShell = function (q, iw, ih) {
    this._clearCells();
    this.answerMode = "shell";
    this.answer.value = "";
    this.answer.setAttribute("class", "ans hidden");
    this.pairWrap.setAttribute("class", "pairs hidden");
    this.angleWrap.setAttribute("class", "angle hidden");
    this.timingWrap.setAttribute("class", "timing hidden");
    this.optionsWrap.setAttribute("class", "options hidden");
    this.orderBar.setAttribute("class", "ordersum hidden");
    this.cellWrap.setAttribute("class", "cells");
    this.cellStatus.setAttribute("class", "cellsum");
    this.auxWrap.setAttribute("class", "aux");
    this.btnUndo.setAttribute("class", "hidden");
    this.btnReset.setAttribute("class", "");
    this.btnRotate.setAttribute("class", "hidden");
    var reduced = !!(global.matchMedia && global.matchMedia("(prefers-reduced-motion: reduce)").matches);
    if (reduced) {
      this._fail("motion-required", "errMotionRequired", null, true);
      return false;
    }
    this.shellChallenge = q;
    this.shellIW = iw;
    this.shellIH = ih;
    this.shellFrameCols = intField(q, "frame_cols");
    this.shellFrameRows = intField(q, "frame_rows");
    this.shellFrameMS = intField(q, "frame_ms");
    this.shellFramesPerRound = intField(q, "frames_per_round");
    this.shellRounds = intField(q, "rounds");
    this.shellSelections = [-1, -1, -1];
    this.shellRound = 0;
    this.shellFrame = 0;
    this._clearImage();
    var view = el("div", { class: "frameview", role: "img", "aria-label": tr(this.o.lang, "imageAltShell") });
    h.spriteStyle(view, iw, ih, this.shellFrameCols, this.shellFrameRows);
    view.style.backgroundImage = "url(data:" + q.image_mime + ";base64," + q.image_base64 + ")";
    this.frameView = view;
    this.imgBox.appendChild(view);
    this._showImageBox();
    var self = this, boxes = q.boxes;
    for (var i = 0; i < boxes.length; i++) {
      var b = boxes[i];
      var btn = el("button", {
        type: "button", class: "cell shellbox", "data-box": String(i),
        "aria-label": tr(this.o.lang, "slotLabel", { n: i + 1 }),
        "aria-describedby": this.note.getAttribute("id") + " " + this.status.getAttribute("id"),
        style: "left:" + (b.x / iw * 100) + "%;top:" + (b.y / ih * 100) + "%;width:" +
          (b.w / iw * 100) + "%;height:" + (b.h / ih * 100) + "%"
      });
      btn.disabled = true;
      var fn = (function (boxIdx) {
        return function (e) { e.preventDefault(); self._shellSelect(boxIdx); };
      })(i);
      btn._epochaClick = fn;
      this._on(btn, "click", fn);
      this.cellButtons.push(btn);
      this.cellWrap.appendChild(btn);
    }
    this._shellShowFrame(0);
    this._shellPlayRound(0);
    return true;
  };


  Widget.prototype._shellShowFrame = function (frame) {
    if (!this.frameView) return;
    this.frameView.style.backgroundPosition = h.spritePosition(frame, this.shellFrameCols, this.shellFrameRows);
  };


  Widget.prototype._shellPlayRound = function (round) {
    this._stopShell();
    if (!this.shellChallenge) return;
    this.shellRound = round;
    this.shellAnimating = true;
    var start = round * this.shellFramesPerRound;
    var end = start + this.shellFramesPerRound - 1;
    this.shellFrame = start;
    this._shellShowFrame(start);
    for (var i = 0; i < this.cellButtons.length; i++) this.cellButtons[i].setAttribute("data-sel", "false");
    if (this.state === STATES.READY) this._setInputsEnabled(true);
    this.cellStatus.textContent = tr(this.o.lang, "shellWatch", { n: round + 1, total: this.shellRounds });
    var self = this, my = this.gen;
    var step = function () {
      if (self.destroyed || my !== self.gen) return;
      self.shellFrame++;
      if (self.shellFrame <= end) {
        self._shellShowFrame(self.shellFrame);
        self.shellTimer = global.setTimeout(step, self.shellFrameMS);
        return;
      }
      self.shellAnimating = false;
      self.shellTimer = 0;
      if (self.state === STATES.READY) {
        self._setInputsEnabled(true);
        self.cellStatus.textContent = tr(self.o.lang, "shellChoose");
        if (self.cellButtons[0] && typeof self.cellButtons[0].focus === "function") self.cellButtons[0].focus();
      }
    };
    this.shellTimer = global.setTimeout(step, this.shellFrameMS);
  };


  Widget.prototype._shellSelect = function (idx) {
    if (this.destroyed || this.state !== STATES.READY || this.answerMode !== "shell" || this.shellAnimating) return;
    if (idx < 0 || idx >= 4) return;
    this.shellSelections[this.shellRound] = idx;
    for (var i = 0; i < this.cellButtons.length; i++) {
      this.cellButtons[i].setAttribute("data-sel", i === idx ? "true" : "false");
    }
    if (this.shellRound < this.shellRounds - 1) {
      this._shellPlayRound(this.shellRound + 1);
      return;
    }
    var parts = [];
    for (var k = 0; k < this.shellSelections.length; k++) parts.push(String(this.shellSelections[k]));
    this.answer.value = "v1:" + parts.join(".");
    this.cellStatus.textContent = tr(this.o.lang, "shellChoose");
    this._updateSubmit();
  };

  // --- silhouette-assembly-v1 ---


  Widget.prototype._renderPlacements = function (q, iw, ih) {
    this._clearCells();
    this.answerMode = "placements";
    this.answer.value = "";
    this.answer.setAttribute("class", "ans hidden");
    this.pairWrap.setAttribute("class", "pairs hidden");
    this.angleWrap.setAttribute("class", "angle hidden");
    this.timingWrap.setAttribute("class", "timing hidden");
    this.optionsWrap.setAttribute("class", "options hidden");
    this.orderBar.setAttribute("class", "ordersum hidden");
    this.cellWrap.setAttribute("class", "cells");
    this.cellStatus.setAttribute("class", "cellsum");
    this.auxWrap.setAttribute("class", "aux");
    this.btnUndo.setAttribute("class", "");
    this.btnReset.setAttribute("class", "");
    this.btnRotate.setAttribute("class", "");
    this.placeChallenge = q;
    this.placeIW = iw;
    this.placeIH = ih;
    this.placeState = [null, null, null, null, null];
    this.placeSelected = "";
    this.placeRotation = 0;
    this.placeHistory = [];
    this.pieceRects = {};
    var self = this;
    for (var i = 0; i < q.pieces.length; i++) {
      var p = q.pieces[i];
      this.pieceRects[p.id] = p;
      var btn = el("button", {
        type: "button", class: "cell piece", "data-piece": p.id,
        "aria-label": tr(this.o.lang, "pieceLabel", { id: p.id }),
        "aria-describedby": this.note.getAttribute("id") + " " + this.status.getAttribute("id"),
        style: "left:" + (p.x / iw * 100) + "%;top:" + (p.y / ih * 100) + "%;width:" +
          (p.w / iw * 100) + "%;height:" + (p.h / ih * 100) + "%"
      });
      var img = el("span", { class: "pieceimg" });
      this._sliceBackground(img, q, iw, ih, p.x, p.y, p.w, p.h);
      btn.appendChild(img);
      btn.setAttribute("aria-pressed", "false");
      var fn = (function (id) {
        return function (e) { e.preventDefault(); self._placeSelectPiece(id); };
      })(p.id);
      btn._epochaClick = fn;
      this._on(btn, "click", fn);
      this.cellButtons.push(btn);
      this.cellWrap.appendChild(btn);
    }
    for (var s = 0; s < q.slots.length; s++) {
      var slot = q.slots[s];
      var sbtn = el("button", {
        type: "button", class: "cell slot", "data-slot": String(s),
        "aria-label": tr(this.o.lang, "slotLabel", { n: s + 1 }),
        "aria-describedby": this.note.getAttribute("id") + " " + this.status.getAttribute("id"),
        style: "left:" + (slot.x / iw * 100) + "%;top:" + (slot.y / ih * 100) + "%;width:" +
          (slot.w / iw * 100) + "%;height:" + (slot.h / ih * 100) + "%"
      });
      var simg = el("span", { class: "pieceimg hidden" });
      sbtn.appendChild(simg);
      var sfn = (function (slotIdx) {
        return function (e) { e.preventDefault(); self._placeSlot(slotIdx); };
      })(s);
      sbtn._epochaClick = sfn;
      this._on(sbtn, "click", sfn);
      this.cellButtons.push(sbtn);
      this.cellWrap.appendChild(sbtn);
    }
    this._syncPlacements();
  };


  Widget.prototype._placeSnapshot = function () {
    var snap = [];
    for (var i = 0; i < this.placeState.length; i++) {
      var v = this.placeState[i];
      snap.push(v ? { id: v.id, rot: v.rot } : null);
    }
    this.placeHistory.push(snap);
    if (this.placeHistory.length > 100) this.placeHistory.shift();
  };


  Widget.prototype._placeSelectPiece = function (id) {
    if (this.destroyed || this.state !== STATES.READY || this.answerMode !== "placements") return;
    if (this.placeSelected === id) {
      this.placeSelected = "";
      this.placeRotation = 0;
      this._syncPlacements();
      return;
    }
    var placedAt = -1;
    for (var i = 0; i < this.placeState.length; i++) {
      if (this.placeState[i] && this.placeState[i].id === id) { placedAt = i; break; }
    }
    if (placedAt >= 0) {
      this._placeSnapshot();
      this.placeState[placedAt] = null;
    }
    this.placeSelected = id;
    this.placeRotation = 0;
    this.cellStatus.textContent = tr(this.o.lang, "pieceSelected", { id: id });
    this._syncPlacements();
  };


  Widget.prototype._placeSlot = function (slot) {
    if (this.destroyed || this.state !== STATES.READY || this.answerMode !== "placements") return;
    if (!this.placeSelected) return;
    this._placeSnapshot();
    for (var i = 0; i < this.placeState.length; i++) {
      if (this.placeState[i] && this.placeState[i].id === this.placeSelected) this.placeState[i] = null;
    }
    this.placeState[slot] = { id: this.placeSelected, rot: this.placeRotation };
    this.placeSelected = "";
    this.placeRotation = 0;
    this._syncPlacements();
  };


  Widget.prototype._placeRotate = function () {
    if (this.destroyed || this.state !== STATES.READY || this.answerMode !== "placements") return;
    if (!this.placeSelected) return;
    this.placeRotation = (this.placeRotation + 1) % 4;
    this.cellStatus.textContent = tr(this.o.lang, "pieceRotated", { deg: this.placeRotation * 90 });
    this._syncPlacements();
  };


  Widget.prototype._placeUndo = function () {
    if (!this.placeHistory.length) return;
    var snap = this.placeHistory.pop();
    for (var i = 0; i < this.placeState.length; i++) {
      this.placeState[i] = snap[i] ? { id: snap[i].id, rot: snap[i].rot } : null;
    }
    this.placeSelected = "";
    this.placeRotation = 0;
    this._syncPlacements();
  };


  Widget.prototype._syncPlacements = function () {
    var q = this.placeChallenge;
    if (!q) return;
    var self = this;
    var placed = {};
    for (var i = 0; i < this.placeState.length; i++) {
      if (this.placeState[i]) placed[this.placeState[i].id] = true;
    }
    for (var k = 0; k < this.cellButtons.length; k++) {
      var btn = this.cellButtons[k];
      var id = btn.getAttribute("data-piece");
      if (id === null) continue;
      btn.setAttribute("aria-pressed", this.placeSelected === id ? "true" : "false");
      btn.setAttribute("data-sel", this.placeSelected === id ? "true" : "false");
      btn.setAttribute("data-used", placed[id] ? "true" : "false");
      var img = btn.firstChild;
      if (img) {
        img.style.transform = (this.placeSelected === id && this.placeRotation)
          ? "rotate(" + (this.placeRotation * 90) + "deg)" : "rotate(0deg)";
      }
    }
    for (var s = 0; s < this.placeState.length; s++) {
      var sbtn = null;
      for (var j = 0; j < this.cellButtons.length; j++) {
        if (this.cellButtons[j].getAttribute("data-slot") === String(s)) { sbtn = this.cellButtons[j]; break; }
      }
      if (!sbtn) continue;
      var simg = sbtn.firstChild;
      var st = this.placeState[s];
      if (st && this.pieceRects[st.id]) {
        var p = this.pieceRects[st.id];
        this._sliceBackground(simg, q, this.placeIW, this.placeIH, p.x, p.y, p.w, p.h);
        simg.setAttribute("class", "pieceimg");
        simg.style.transform = st.rot ? "rotate(" + (st.rot * 90) + "deg)" : "rotate(0deg)";
      } else if (simg) {
        simg.setAttribute("class", "pieceimg hidden");
      }
    }
    var filled = 0;
    for (var f = 0; f < this.placeState.length; f++) if (this.placeState[f]) filled++;
    this.cellStatus.textContent = tr(this.o.lang, "placementsProgress", { n: filled });
    if (filled === this.placeState.length) {
      var parts = [];
      for (var s2 = 0; s2 < this.placeState.length; s2++) {
        parts.push(this.placeState[s2].id + ":" + this.placeState[s2].rot);
      }
      this.answer.value = "v1:" + parts.join(",");
    } else {
      this.answer.value = "";
    }
    if (this.btnRotate) this.btnRotate.disabled = !(this.placeSelected && this.state === STATES.READY);
    this._updateSubmit();
  };

  });
})(typeof window !== "undefined" ? window : globalThis);
