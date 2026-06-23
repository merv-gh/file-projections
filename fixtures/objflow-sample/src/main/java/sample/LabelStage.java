package sample;

public class LabelStage {
    public void apply(Receipt r, int amount) {
        int net = amount - amount / 10;
        r.setLabel("$" + amount);
    }
}
