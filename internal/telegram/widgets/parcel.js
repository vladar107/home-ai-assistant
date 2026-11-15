  var carrier = document.getElementById('carrier');
  var title = document.getElementById('title');
  var sender = document.getElementById('sender');
  var state = document.getElementById('state');
  var when = document.getElementById('when');
  var pickup = document.getElementById('pickup');
  var actions = document.getElementById('actions');

  function stateText(data) {
    var mode = String(data.m || '');
    if (mode === 'x') {
      return label('stopped');
    }
    if (mode === 'o') {
      return label('off');
    }
    if (mode === 'i') {
      return label('idle');
    }
    var words = label('statuses') || {};
    return words[String(data.s || '')] || words.unknown || '';
  }

  function labelled(key, value) {
    return value ? label(key) + ' ' + String(value) : '';
  }

  function draw(data) {
    var open = !data.m && data.s !== 'delivered';
    carrier.textContent = String(data.c || '');
    title.textContent = String(data.t || '');
    sender.textContent = labelled('from', data.f);
    state.textContent = stateText(data);
    when.textContent = open ? labelled('expected', data.w) : '';
    pickup.textContent = labelled('pickup', data.p);
    while (actions.firstChild) {
      actions.removeChild(actions.firstChild);
    }
    if (open && data.i) {
      var stop = document.createElement('button');
      stop.className = 'stop';
      stop.textContent = label('stop');
      stop.onclick = function () {
        request('parcel:stop:' + String(data.i), dataChanges);
      };
      actions.appendChild(stop);
    }
    actions.style.display = actions.firstChild ? 'flex' : 'none';
  }
