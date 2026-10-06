package explain

// pageCSS and quizJS are the page's whole presentation layer, ported from the
// explain-diff recipe's render.py. They live here rather than in the prompts
// so that regenerating a page never regenerates the boilerplate, and so the
// style is identical on every page this tool has ever written.

// The warm paper palette, a serif body (the explanation is meant to be read,
// not skimmed), and a monospace dark block for code. The .diagram/.flow/.box
// classes are the vocabulary the chunk prompt teaches the model: a figure is
// wrapped in .diagram, a row of steps in .flow, each step a .box, the
// connector an .arrow, and the failure path a .box.fail.
const pageCSS = `
:root {
  --bg: #fafaf8; --fg: #1a1a1a; --accent: #b5541f; --muted: #6b6b6b;
  --code-bg: #282c34; --code-fg: #e6e6e6; --callout-bg: #fff4e8; --border: #e0ddd6;
  --ok: #16a34a; --ok-bg: #ecfdf3; --ok-fg: #166534; --bad: #dc2626; --bad-bg: #fef2f2; --bad-fg: #991b1b;
}
body { font-family: Georgia, 'Times New Roman', serif; background: var(--bg); color: var(--fg);
  max-width: 820px; margin: 0 auto; padding: 2rem 1.5rem 6rem; line-height: 1.65; }
h1 { font-size: 1.9rem; border-bottom: 3px solid var(--accent); padding-bottom: .5rem; }
h2 { font-size: 1.4rem; margin-top: 3rem; color: var(--accent); }
h3 { font-size: 1.1rem; margin-top: 1.8rem; }
.subtitle { color: var(--muted); margin-top: -.5rem; font-style: italic; }
code { font-family: 'SF Mono', Consolas, monospace; background: #eee; padding: .1rem .3rem; border-radius: 3px; font-size: .92em; }
pre { background: var(--code-bg); color: var(--code-fg); padding: 1rem 1.2rem; border-radius: 8px;
  overflow-x: auto; white-space: pre-wrap; font-family: 'SF Mono', Consolas, monospace; font-size: .88rem; line-height: 1.5; }
pre code { background: none; padding: 0; color: inherit; }
.callout { background: var(--callout-bg); border-left: 4px solid var(--accent); padding: .9rem 1.2rem;
  border-radius: 0 6px 6px 0; margin: 1.2rem 0; }
.toc { background: #fff; border: 1px solid var(--border); border-radius: 8px; padding: 1rem 1.5rem; margin: 1.5rem 0; }
.toc a { color: var(--accent); text-decoration: none; }
.toc ul { margin: .3rem 0; }
.diagram { background: #fff; border: 1px solid var(--border); border-radius: 10px; padding: 1.2rem;
  margin: 1.2rem 0; font-family: 'SF Mono', Consolas, monospace; font-size: .85rem; }
.flow { display: flex; align-items: center; gap: .6rem; flex-wrap: wrap; justify-content: center; padding: .5rem 0; }
.box { border: 2px solid var(--accent); border-radius: 8px; padding: .6rem 1rem; background: #fdf6ee; text-align: center; min-width: 120px; }
.box.fail { border-color: var(--bad); background: var(--bad-bg); }
.arrow { font-size: 1.4rem; color: var(--muted); }
table { border-collapse: collapse; width: 100%; margin: 1rem 0; font-size: .92rem; }
th, td { border: 1px solid var(--border); padding: .5rem .7rem; text-align: left; }
th { background: #f0ede6; }
.quiz-q { background: #fff; border: 1px solid var(--border); border-radius: 10px; padding: 1.2rem 1.5rem; margin: 1.2rem 0; }
.q-text { font-weight: bold; margin-top: 0; }
.score { font-family: -apple-system, 'Segoe UI', sans-serif; font-size: .9rem; color: var(--muted);
  border: 1px solid var(--border); background: #fff; border-radius: 8px; padding: .4rem .8rem; display: inline-block; }
.quiz-opt { display: block; width: 100%; text-align: left; padding: .6rem 1rem; margin: .4rem 0;
  border: 1px solid var(--border); border-radius: 6px; background: #fff; cursor: pointer; font-family: inherit; font-size: .95rem; }
.quiz-opt:hover:not(:disabled) { background: #f5f2ec; }
.quiz-opt:disabled { cursor: default; opacity: .95; }
.quiz-opt.picked-correct { background: var(--ok-bg); border-color: var(--ok); }
.quiz-opt.picked-wrong { background: var(--bad-bg); border-color: var(--bad); }
.feedback { display: none; margin-top: .6rem; padding: .6rem 1rem; border-radius: 6px; font-size: .9rem; }
.feedback.correct { background: var(--ok-bg); color: var(--ok-fg); border-left: 3px solid var(--ok); }
.feedback.incorrect { background: var(--bad-bg); color: var(--bad-fg); border-left: 3px solid var(--bad); }
.footer { color: var(--muted); font-size: .8rem; border-top: 1px solid var(--border); margin-top: 3.5rem; padding-top: .8rem; }
@media (max-width: 600px) { body { padding: 1rem; } .flow { flex-direction: column; } .arrow { transform: rotate(90deg); } }
`

// quizJS grades the quiz in the page: the first click on a question locks it
// in, reveals that option's feedback, and updates the running score. The
// original recipe only said correct or not-quite; the feedback is the part of
// the quiz that actually teaches, and the Notion variant of the skill spells
// out why each option is right or wrong.
const quizJS = `
(function () {
  var questions = document.querySelectorAll('.quiz-q');
  var scoreEl = document.getElementById('quiz-score');
  var answered = 0, correct = 0;

  function updateScore() {
    if (!scoreEl) return;
    scoreEl.textContent = answered === 0
      ? 'Score: 0 / ' + questions.length + ' — ' + questions.length + ' to go'
      : 'Score: ' + correct + ' / ' + questions.length + ' — ' + (questions.length - answered) + ' to go';
  }

  questions.forEach(function (q) {
    var options = q.querySelectorAll('.quiz-opt');
    options.forEach(function (opt) {
      opt.addEventListener('click', function () {
        if (q.dataset.answered === 'true') return;   // one answer per question
        q.dataset.answered = 'true';
        var isCorrect = opt.dataset.correct === 'true';
        answered++;
        if (isCorrect) correct++;

        options.forEach(function (o) {
          o.disabled = true;
          o.style.opacity = '0.6';
        });
        opt.style.opacity = '1';
        opt.classList.add(isCorrect ? 'picked-correct' : 'picked-wrong');

        var fb = document.createElement('div');
        fb.className = 'feedback ' + (isCorrect ? 'correct' : 'incorrect');
        var mark = isCorrect ? '✅ Correct. ' : '❌ Not quite. ';
        var why = opt.dataset.feedback || (isCorrect ? '' : 'Reread the section above.');
        fb.textContent = mark + why;
        opt.insertAdjacentElement('afterend', fb);
        fb.style.display = 'block';

        updateScore();
      });
    });
  });

  updateScore();
})();
`
