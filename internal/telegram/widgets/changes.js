  var heading = document.getElementById('heading');
  var count = document.getElementById('count');
  var list = document.getElementById('list');
  var state = document.getElementById('state');
  var warning = document.getElementById('warning');
  var actions = document.getElementById('actions');
  var unticked = {};
  var opened = {};
  var answers = {};

  function clear(element) {
    while (element.firstChild) {
      element.removeChild(element.firstChild);
    }
  }

  function element(tag, className, text) {
    var made = document.createElement(tag);
    if (className) {
      made.className = className;
    }
    if (text !== undefined) {
      made.textContent = text;
    }
    return made;
  }

  function isIgnored(answer) {
    for (var code in labels) {
      if (labels[code].ignored === answer) {
        return true;
      }
    }
    return false;
  }

  function stateText(change) {
    var words = label('words');
    var code = String(change.s || '');
    if (words[code]) {
      if (code === 'f') {
        return change.o ? String(change.o) : words[code];
      }
      return code === 'd' && change.o ? words[code] + '. ' + change.o : words[code];
    }
    return String(change.o || '');
  }

  function fullText(change, index) {
    if (change.x) {
      return String(change.x);
    }
    return answers[index] || '';
  }

  function toggle(data, index) {
    if (opened[index]) {
      opened[index] = false;
      draw(data);
      return;
    }
    var change = data.c[index];
    if (fullText(change, index)) {
      opened[index] = true;
      draw(data);
      return;
    }
    request('approval:read:' + data.i + ':' + (index + 1), function (answer) {
      if (isIgnored(answer)) {
        status.textContent = answer;
      } else if (answer) {
        answers[index] = answer;
        opened[index] = true;
      }
      draw(current());
    });
  }

  function row(data, index) {
    var change = data.c[index];
    var line = element('div', 'row');
    var tick = element('div', 'tick');
    if (change.s === 'w') {
      tick.className = unticked[index] ? 'tick' : 'tick on';
      tick.textContent = unticked[index] ? '' : '✓';
      tick.onclick = function () {
        if (busy) {
          return;
        }
        unticked[index] = !unticked[index];
        draw(data);
      };
    } else {
      tick.className = 'tick off';
    }
    var name = element('div', 'name');
    var title = element('b', '', (change.w ? '⚠️ ' : '') + (index + 1) + '. ' + String(change.t || ''));
    title.onclick = function () {
      if (!busy) {
        toggle(data, index);
      }
    };
    name.appendChild(title);
    var note = change.s === 'w' ? '' : stateText(change);
    if (note) {
      name.appendChild(element('div', 'note', note));
    }
    if (opened[index]) {
      name.appendChild(element('div', 'full', fullText(change, index)));
    }
    line.appendChild(tick);
    line.appendChild(name);
    return line;
  }

  function control(label, className, data, after, off) {
    var button = element('button', className, label);
    if (off) {
      button.setAttribute('data-off', '1');
      button.disabled = true;
    }
    button.onclick = function () {
      request(data, after);
    };
    return button;
  }

  function draw(data) {
    var changes = data.c instanceof Array ? data.c : [];
    var waiting = false;
    var marked = false;
    var kept = 0;
    var open = 0;
    var flags = '';
    clear(list);
    for (var i = 0; i < changes.length; i++) {
      if (!changes[i] || typeof changes[i] !== 'object') {
        flags += '1';
        continue;
      }
      if (changes[i].s === 'w') {
        waiting = true;
        open++;
        if (!unticked[i]) {
          kept++;
        }
      }
      if (changes[i].w) {
        marked = true;
      }
      flags += unticked[i] ? '0' : '1';
      list.appendChild(row(data, i));
    }
    heading.textContent = String(data.h || '');
    count.textContent = label('counts')[changes.length] || changes.length + ' changes';
    state.textContent = String(data.s || '');
    warning.textContent = marked ? label('warning') : '';
    clear(actions);
    if (waiting) {
      actions.appendChild(control(label('confirm').replace('{1}', kept).replace('{2}', open), '', 'approval:keep:' + data.i + ':' + flags, dataChanges, kept === 0));
      actions.appendChild(control(label('rejectAll'), 'reject', 'approval:reject:' + data.i, dataChanges, false));
      if (data.p) {
        actions.appendChild(control(label('preview'), 'quiet', 'approval:preview:' + data.i, controlsReturn, false));
      }
    }
    actions.style.display = actions.firstChild ? 'flex' : 'none';
  }
