docker run dnonakolesax/noted-runner \
    -v noted-codes:/noted/codes \
    --network noted-rmq-runners \
    --env-file=kernel.env 