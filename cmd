docker run -v notedcode:/noted/codes \
    --network noted-rmq-runners \
    --env-file=kernel-example.env \ 
    dnonakolesax/noted-kernel:0.0.1