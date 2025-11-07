docker run -v notedcode:/noted/codes \
    --network noted-rmq-runners \
    --env-file=kernel-example.env \
    -p 8080:8080 \
    --rm \ 
    dnonakolesax/noted-kernel:0.0.1