  var title = document.getElementById('title');
  var text = document.getElementById('text');
  var state = document.getElementById('state');
  var actions = document.getElementById('actions');

  function button(action) {
    var element = document.createElement('button');
    var data = String(action.data || '');
    var preview = data.indexOf('approval:preview:') === 0;
    element.textContent = String(action.label || '');
    if (preview) {
      element.className = 'quiet';
    } else if (data.indexOf('approval:reject:') === 0) {
      element.className = 'reject';
    }
    element.onclick = function () {
      request(data, preview ? controlsReturn : dataChanges);
    };
    return element;
  }

  function draw(data) {
    var list = data.actions instanceof Array ? data.actions : [];
    var whole = String(data.text || '');
    var lineEnd = whole.indexOf('\n');
    title.textContent = lineEnd < 0 ? whole : whole.substring(0, lineEnd);
    text.textContent = lineEnd < 0 ? '' : whole.substring(lineEnd + 1).replace(/^\s+/, '');
    state.textContent = String(data.status || '');
    while (actions.firstChild) {
      actions.removeChild(actions.firstChild);
    }
    for (var i = 0; i < list.length; i++) {
      actions.appendChild(button(list[i]));
    }
    actions.style.display = list.length ? 'flex' : 'none';
  }
