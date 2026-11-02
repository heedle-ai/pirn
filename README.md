# pirn

Claude for agent steps: `(ask "instruction")` answers from the step's inputs, and the answer is recorded with them. Part of [heedle-ai](https://github.com/heedle-ai).

```lisp
(pipe (par (browse "https://news.ycombinator.com/") (browse "https://lobste.rs/"))
      (ask "What are today's three most important engineering stories? Cite the source of each."))
```

Documentation is in the [wiki](https://github.com/heedle-ai/pirn/wiki) (source: `docs/wiki/`).
